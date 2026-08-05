//! Sync & apply engine (CLIENT.md §4, normative).
//!
//! plan → download-to-staging (Range resume + streaming SHA-256, concurrency 4)
//! → commit (rename into instance/, then delete removed) → atomic state.json.
//! Player extras (files unknown to both state and target) are never touched.

use std::collections::{BTreeMap, BTreeSet};
use std::path::Path;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::UNIX_EPOCH;

use anyhow::{anyhow, Context, Result};
use futures_util::StreamExt;
use sha2::{Digest, Sha256};
use tokio::io::{AsyncReadExt, AsyncWriteExt};

use crate::api::DaemonClient;
use crate::state::{AppCore, FileEntry, LocalState, Manifest, Paths, Progress};

const CONCURRENCY: usize = 4;

/// A file that must be downloaded.
#[derive(Clone)]
struct Need {
    path: String,
    sha256: String,
    size: u64,
}

struct Plan {
    downloads: Vec<Need>,
    deletes: Vec<String>,
    /// Cache entries for files already correct on disk (carried into new state.json).
    keep: BTreeMap<String, FileEntry>,
    download_bytes: u64,
}

/// True for files the client materializes (`side ∈ {client, both}`).
fn client_side(side: &str) -> bool {
    side == "client" || side == "both"
}

fn mtime_ms(meta: &std::fs::Metadata) -> u64 {
    meta.modified()
        .ok()
        .and_then(|t| t.duration_since(UNIX_EPOCH).ok())
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0)
}

/// Hash a file on disk, returning (hex sha256, size, mtime_ms).
async fn hash_file(path: &Path) -> Result<(String, u64, u64)> {
    let mut f = tokio::fs::File::open(path).await?;
    let meta = f.metadata().await?;
    let mut hasher = Sha256::new();
    let mut buf = vec![0u8; 128 * 1024];
    loop {
        let n = f.read(&mut buf).await?;
        if n == 0 {
            break;
        }
        hasher.update(&buf[..n]);
    }
    Ok((hex::encode(hasher.finalize()), meta.len(), mtime_ms(&meta)))
}

/// Build the plan (CLIENT.md §4 step 1).
async fn build_plan(target: &Manifest, state: &LocalState, instance: &Path) -> Result<Plan> {
    let mut downloads = Vec::new();
    let mut keep: BTreeMap<String, FileEntry> = BTreeMap::new();
    let mut download_bytes = 0u64;
    let mut target_paths: BTreeSet<String> = BTreeSet::new();

    for f in target.files.iter().filter(|f| client_side(&f.side)) {
        target_paths.insert(f.path.clone());
        let disk = instance.join(&f.path);

        if let Some(cached) = state.files.get(&f.path) {
            if cached.sha256 == f.sha256 {
                // Fast path: trust the size+mtime cache.
                if let Ok(meta) = std::fs::metadata(&disk) {
                    if meta.len() == cached.size && mtime_ms(&meta) == cached.mtime_ms {
                        keep.insert(f.path.clone(), cached.clone());
                        continue;
                    }
                    // size/mtime drifted → re-hash the disk file.
                    if let Ok((h, size, mt)) = hash_file(&disk).await {
                        if h == f.sha256 {
                            keep.insert(
                                f.path.clone(),
                                FileEntry { sha256: h, size, mtime_ms: mt },
                            );
                            continue;
                        }
                    }
                }
            }
        }
        // No valid cache entry (or the cache no longer matches). Before scheduling a
        // download, check whether the disk file already has the right content and
        // just re-hash it — this makes "Re-verify all files" (which drops the cache)
        // re-verify by hashing instead of re-downloading, and never touches correct
        // files. Missing files fall through to the download list.
        if let Ok((h, size, mt)) = hash_file(&disk).await {
            if h == f.sha256 {
                keep.insert(f.path.clone(), FileEntry { sha256: h, size, mtime_ms: mt });
                continue;
            }
        }

        // Content missing or wrong → download.
        downloads.push(Need {
            path: f.path.clone(),
            sha256: f.sha256.clone(),
            size: f.size,
        });
        download_bytes += f.size;
    }

    // Anything in the local cache no longer present in the (client-side) target → delete.
    let deletes: Vec<String> = state
        .files
        .keys()
        .filter(|p| !target_paths.contains(*p))
        .cloned()
        .collect();

    Ok(Plan {
        downloads,
        deletes,
        keep,
        download_bytes,
    })
}

/// Shared, live-updated progress counters.
struct Counters {
    bytes_done: AtomicU64,
    bytes_total: u64,
    files_done: AtomicU64,
    files_total: u64,
}

impl Counters {
    fn snapshot(&self, message: Option<String>) -> Progress {
        Progress {
            bytes_done: self.bytes_done.load(Ordering::Relaxed),
            bytes_total: self.bytes_total,
            files_done: self.files_done.load(Ordering::Relaxed),
            files_total: self.files_total,
            message,
        }
    }
}

/// One download attempt from the current partial offset; returns Ok(true) on
/// verified content, Ok(false) on hash mismatch. Rolls its streamed-byte count
/// back into `counters` on failure so the progress bar stays monotonic.
async fn try_fetch(
    client: &DaemonClient,
    need: &Need,
    dest: &Path,
    core: &AppCore,
    counters: &Counters,
) -> Result<bool> {
    let mut hasher = Sha256::new();
    let mut start = 0u64;

    // Resume: seed the hasher with any valid partial bytes.
    if let Ok(meta) = tokio::fs::metadata(dest).await {
        if meta.len() > 0 && meta.len() <= need.size {
            let mut existing = tokio::fs::File::open(dest).await?;
            let mut buf = vec![0u8; 128 * 1024];
            loop {
                let n = existing.read(&mut buf).await?;
                if n == 0 {
                    break;
                }
                hasher.update(&buf[..n]);
            }
            start = meta.len();
        } else if meta.len() > need.size {
            let _ = tokio::fs::remove_file(dest).await;
        }
    }

    let resp = client
        .blob(&need.sha256, if start > 0 { Some(start) } else { None })
        .await
        .map_err(|e| anyhow!("blob {} : {}", need.sha256, e))?;

    let mut file = tokio::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(dest)
        .await
        .with_context(|| format!("open staging {}", dest.display()))?;

    let mut streamed_this_attempt = 0u64;
    let mut stream = resp.bytes_stream();
    let mut last_emit = std::time::Instant::now();
    while let Some(chunk) = stream.next().await {
        let chunk = match chunk {
            Ok(c) => c,
            Err(e) => {
                // Roll back this attempt's byte count before bubbling up.
                counters
                    .bytes_done
                    .fetch_sub(streamed_this_attempt, Ordering::Relaxed);
                return Err(anyhow!("stream error: {}", e));
            }
        };
        file.write_all(&chunk).await?;
        hasher.update(&chunk);
        streamed_this_attempt += chunk.len() as u64;
        counters.bytes_done.fetch_add(chunk.len() as u64, Ordering::Relaxed);
        // Throttle progress emits to ~10/s.
        if last_emit.elapsed().as_millis() > 100 {
            core.emit_progress(&counters.snapshot(None));
            last_emit = std::time::Instant::now();
        }
    }
    file.flush().await?;

    let digest = hex::encode(hasher.finalize());
    if digest == need.sha256 {
        Ok(true)
    } else {
        counters
            .bytes_done
            .fetch_sub(streamed_this_attempt, Ordering::Relaxed);
        Ok(false)
    }
}

/// Download + verify one blob into staging, retrying once on mismatch/error.
async fn fetch_verified(
    client: &DaemonClient,
    need: &Need,
    staging: &Path,
    core: &AppCore,
    counters: &Counters,
) -> Result<()> {
    let dest = staging.join(&need.sha256);
    let mut last_err: Option<anyhow::Error> = None;
    for attempt in 0..2 {
        match try_fetch(client, need, &dest, core, counters).await {
            Ok(true) => {
                counters.files_done.fetch_add(1, Ordering::Relaxed);
                core.emit_progress(&counters.snapshot(None));
                return Ok(());
            }
            Ok(false) => {
                let _ = tokio::fs::remove_file(&dest).await;
                last_err = Some(anyhow!("SHA-256 mismatch for {}", need.sha256));
            }
            Err(e) => {
                let _ = tokio::fs::remove_file(&dest).await;
                last_err = Some(e);
            }
        }
        let _ = attempt;
    }
    Err(last_err.unwrap_or_else(|| anyhow!("download failed: {}", need.sha256)))
}

/// Rename a verified staged blob into its instance path (CLIENT.md §4 step 3).
async fn commit_file(staging: &Path, instance: &Path, need: &Need) -> Result<FileEntry> {
    let src = staging.join(&need.sha256);
    let dst = instance.join(&need.path);
    if let Some(parent) = dst.parent() {
        tokio::fs::create_dir_all(parent).await?;
    }
    if let Err(e) = tokio::fs::rename(&src, &dst).await {
        // On Windows a locked file (game running) surfaces here; the commit is
        // per-file idempotent and re-planned on retry, so nothing is half-applied.
        #[cfg(windows)]
        {
            return Err(anyhow!("Close Minecraft first (locked: {})", need.path));
        }
        #[cfg(not(windows))]
        {
            // Fallback to copy+remove across filesystems.
            tokio::fs::copy(&src, &dst)
                .await
                .with_context(|| format!("commit {} : {}", need.path, e))?;
            let _ = tokio::fs::remove_file(&src).await;
        }
    }
    let meta = tokio::fs::metadata(&dst).await?;
    Ok(FileEntry {
        sha256: need.sha256.clone(),
        size: meta.len(),
        mtime_ms: {
            let std_meta = std::fs::metadata(&dst)?;
            mtime_ms(&std_meta)
        },
    })
}

/// Apply `target` to the managed instance. Emits progress throughout; on success
/// writes state.json atomically and returns the applied version number.
pub async fn apply(core: &AppCore, client: &DaemonClient, target: &Manifest) -> Result<u64> {
    let paths: Paths = core.paths.clone();
    let instance = paths.instance();
    let staging = paths.staging();
    tokio::fs::create_dir_all(&instance).await?;
    tokio::fs::create_dir_all(&staging).await?;

    let state = crate::state::load_state(&paths);
    let plan = build_plan(target, &state, &instance).await?;

    let counters = Arc::new(Counters {
        bytes_done: AtomicU64::new(0),
        bytes_total: plan.download_bytes,
        files_done: AtomicU64::new(0),
        files_total: plan.downloads.len() as u64,
    });
    core.emit_progress(&counters.snapshot(Some("Planning complete".into())));

    // --- Download all (concurrency 4). Commit only after every blob verifies. ---
    let results: Vec<Result<()>> = futures_util::stream::iter(plan.downloads.iter().cloned())
        .map(|need| {
            let counters = counters.clone();
            let staging = staging.clone();
            async move { fetch_verified(client, &need, &staging, core, &counters).await }
        })
        .buffer_unordered(CONCURRENCY)
        .collect()
        .await;

    for r in results {
        r?; // any download failure aborts before commit → nothing applied.
    }

    // --- Commit: rename staged blobs into place. ---
    let mut new_files = plan.keep;
    for need in &plan.downloads {
        let entry = commit_file(&staging, &instance, need).await?;
        new_files.insert(need.path.clone(), entry);
    }

    // --- Deletes (removed files; player extras untouched). ---
    for path in &plan.deletes {
        let p = instance.join(path);
        let _ = tokio::fs::remove_file(&p).await;
    }

    // --- Persist state.json atomically. ---
    let new_state = LocalState {
        schema_version: 1,
        applied_version: Some(target.version.number),
        files: new_files,
    };
    crate::state::save_state(&paths, &new_state)?;

    core.emit_progress(&counters.snapshot(Some("Up to date".into())));
    Ok(target.version.number)
}
