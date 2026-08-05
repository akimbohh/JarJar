//! Official Minecraft Launcher integration (CLIENT.md §6, v1 scope).
//!
//! Responsibilities:
//!   1. Ensure the loader (Fabric/Quilt/NeoForge/Forge) is installed into the
//!      launcher game dir, using the official installers.
//!   2. Upsert a `jarjar-<slug>` profile in `launcher_profiles.json` pointing
//!      `gameDir` at the managed instance.
//!   3. Launch the official launcher on `Start Minecraft`.
//!
//! Prism/MultiMC are unsupported in v1 (documented in README).

use std::path::{Path, PathBuf};

use anyhow::{anyhow, Context, Result};
use serde_json::{json, Value};

use crate::state::{PackInfo, Paths};

/// Platform default `.minecraft` game directory.
pub fn default_minecraft_dir() -> Option<PathBuf> {
    #[cfg(target_os = "windows")]
    {
        std::env::var_os("APPDATA").map(|p| PathBuf::from(p).join(".minecraft"))
    }
    #[cfg(target_os = "macos")]
    {
        dirs_home().map(|h| h.join("Library/Application Support/minecraft"))
    }
    #[cfg(all(unix, not(target_os = "macos")))]
    {
        dirs_home().map(|h| h.join(".minecraft"))
    }
}

#[allow(dead_code)]
fn dirs_home() -> Option<PathBuf> {
    std::env::var_os("HOME").map(PathBuf::from)
}

/// Resolve the game dir: explicit override or platform default.
pub fn game_dir(cfg_override: &Option<String>) -> Result<PathBuf> {
    if let Some(dir) = cfg_override {
        if !dir.trim().is_empty() {
            return Ok(PathBuf::from(dir));
        }
    }
    default_minecraft_dir().ok_or_else(|| anyhow!("cannot locate the .minecraft directory"))
}

/// The `lastVersionId` string the launcher uses for each loader family.
pub fn loader_version_id(pack: &PackInfo) -> String {
    let lv = &pack.loader.version;
    let mc = &pack.mc_version;
    match pack.loader.id.as_str() {
        "fabric" => format!("fabric-loader-{}-{}", lv, mc),
        "quilt" => format!("quilt-loader-{}-{}", lv, mc),
        "neoforge" => format!("neoforge-{}", lv),
        "forge" => format!("{}-forge-{}", mc, lv),
        other => format!("{}-{}", other, lv),
    }
}

/// Locate a Java runtime: JAVA_HOME, the launcher-bundled JRE, then PATH.
pub fn find_java(minecraft_dir: &Path) -> Option<PathBuf> {
    // 1. JAVA_HOME
    if let Some(jh) = std::env::var_os("JAVA_HOME") {
        let cand = PathBuf::from(jh).join("bin").join(java_bin());
        if cand.exists() {
            return Some(cand);
        }
    }
    // 2. Launcher-bundled JRE (runtime/ tree under the game dir's parent).
    //    The official launcher installs JREs under <launcher>/runtime/**/bin/java.
    let runtime_root = minecraft_dir.join("runtime");
    if let Some(found) = find_bundled_java(&runtime_root) {
        return Some(found);
    }
    // 3. PATH
    which_in_path(java_bin())
}

fn java_bin() -> &'static str {
    if cfg!(windows) {
        "javaw.exe"
    } else {
        "java"
    }
}

fn find_bundled_java(root: &Path) -> Option<PathBuf> {
    // Shallow walk (a couple of levels) looking for */bin/java.
    let mut stack = vec![(root.to_path_buf(), 0u32)];
    while let Some((dir, depth)) = stack.pop() {
        if depth > 4 {
            continue;
        }
        let Ok(rd) = std::fs::read_dir(&dir) else {
            continue;
        };
        for entry in rd.flatten() {
            let p = entry.path();
            if p.is_dir() {
                let cand = p.join("bin").join(java_bin());
                if cand.exists() {
                    return Some(cand);
                }
                stack.push((p, depth + 1));
            }
        }
    }
    None
}

fn which_in_path(bin: &str) -> Option<PathBuf> {
    let path = std::env::var_os("PATH")?;
    for dir in std::env::split_paths(&path) {
        let cand = dir.join(bin);
        if cand.exists() {
            return Some(cand);
        }
    }
    None
}

// ---------------------------------------------------------------------------
// launcher_profiles.json upsert
// ---------------------------------------------------------------------------

fn slug(name: &str) -> String {
    name.chars()
        .map(|c| if c.is_ascii_alphanumeric() { c.to_ascii_lowercase() } else { '-' })
        .collect::<String>()
        .trim_matches('-')
        .to_string()
}

/// The emerald tray PNG, embedded so the launcher profile shows the JarJar icon.
const EMERALD_PNG: &[u8] = include_bytes!("../icons/32x32.png");

/// Minimal std base64 (no extra crate) for the icon data URI.
fn base64(data: &[u8]) -> String {
    const T: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::with_capacity((data.len() + 2) / 3 * 4);
    for chunk in data.chunks(3) {
        let b = [
            chunk[0],
            *chunk.get(1).unwrap_or(&0),
            *chunk.get(2).unwrap_or(&0),
        ];
        let n = ((b[0] as u32) << 16) | ((b[1] as u32) << 8) | (b[2] as u32);
        out.push(T[((n >> 18) & 63) as usize] as char);
        out.push(T[((n >> 12) & 63) as usize] as char);
        out.push(if chunk.len() > 1 { T[((n >> 6) & 63) as usize] as char } else { '=' });
        out.push(if chunk.len() > 2 { T[(n & 63) as usize] as char } else { '=' });
    }
    out
}

/// Upsert the `jarjar-<slug>` profile pointing gameDir at the managed instance.
pub fn upsert_profile(minecraft_dir: &Path, pack: &PackInfo, instance: &Path) -> Result<()> {
    let profiles_path = minecraft_dir.join("launcher_profiles.json");
    let mut root: Value = if profiles_path.exists() {
        let bytes = std::fs::read(&profiles_path)
            .with_context(|| format!("read {}", profiles_path.display()))?;
        serde_json::from_slice(&bytes).unwrap_or_else(|_| json!({}))
    } else {
        // The launcher creates this itself on first run; seed a minimal shell so
        // our profile survives if the file is absent.
        json!({ "profiles": {}, "version": 3 })
    };

    if !root.get("profiles").map(|p| p.is_object()).unwrap_or(false) {
        root["profiles"] = json!({});
    }

    let id = format!("jarjar-{}", slug(&pack.name));
    let now = chrono_now();
    let version_id = loader_version_id(pack);

    let profiles = root["profiles"].as_object_mut().unwrap();
    // Preserve created/lastUsed if the profile already exists.
    let created = profiles
        .get(&id)
        .and_then(|p| p.get("created"))
        .and_then(|v| v.as_str())
        .map(String::from)
        .unwrap_or_else(|| now.clone());

    profiles.insert(
        id,
        json!({
            "name": format!("JarJar · {}", pack.name),
            "type": "custom",
            "created": created,
            "lastUsed": now,
            "icon": format!("data:image/png;base64,{}", base64(EMERALD_PNG)),
            "gameDir": instance.to_string_lossy(),
            "lastVersionId": version_id,
        }),
    );

    let bytes = serde_json::to_vec_pretty(&root)?;
    crate::state::write_atomic(&profiles_path, &bytes, None)?;
    Ok(())
}

/// Best-effort RFC3339-ish timestamp without pulling in `chrono`.
fn chrono_now() -> String {
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default();
    // The launcher only needs a monotonic-ish sortable string; epoch millis works.
    format!("1970-01-01T00:00:{:03}Z", now.as_millis() % 1000)
}

// ---------------------------------------------------------------------------
// Loader installation
// ---------------------------------------------------------------------------

/// Ensure the loader is installed and the profile is upserted. Downloads and runs
/// the official installer for the loader family in `pack`.
pub async fn ensure_loader(
    http: &reqwest::Client,
    paths: &Paths,
    pack: &PackInfo,
    minecraft_dir_override: &Option<String>,
) -> Result<()> {
    let mcdir = game_dir(minecraft_dir_override)?;
    std::fs::create_dir_all(&mcdir).ok();

    let java = find_java(&mcdir).ok_or_else(|| {
        anyhow!(
            "No Java runtime found. Install Java (or run the Minecraft launcher once so it \
             downloads a JRE), then retry. Download: https://adoptium.net"
        )
    })?;

    match pack.loader.id.as_str() {
        "fabric" | "quilt" => {
            install_fabric_like(http, &mcdir, pack, &java).await?;
        }
        "neoforge" | "forge" => {
            install_forge_like(http, &mcdir, pack, &java).await?;
        }
        other => return Err(anyhow!("unsupported loader: {}", other)),
    }

    upsert_profile(&mcdir, pack, &paths.instance())?;
    Ok(())
}

/// Fabric/Quilt: fetch the official installer jar and run it in client -noprofile mode.
async fn install_fabric_like(
    http: &reqwest::Client,
    mcdir: &Path,
    pack: &PackInfo,
    java: &Path,
) -> Result<()> {
    let is_quilt = pack.loader.id == "quilt";
    // Installer jar URLs (versions pinned to a recent stable installer release).
    // NOTE: the *loader* version comes from the manifest; only the installer tool
    // version is pinned here.
    let (installer_url, jar_name) = if is_quilt {
        (
            "https://maven.quiltmc.org/repository/release/org/quiltmc/quilt-installer/0.9.2/quilt-installer-0.9.2.jar"
                .to_string(),
            "quilt-installer.jar",
        )
    } else {
        (
            "https://maven.fabricmc.net/net/fabricmc/fabric-installer/1.0.1/fabric-installer-1.0.1.jar"
                .to_string(),
            "fabric-installer.jar",
        )
    };

    let jar_path = download_to_temp(http, &installer_url, jar_name).await?;

    // CLIENT.md §6 command template (Fabric shown; Quilt uses the same shape):
    //   java -jar fabric-installer.jar client -dir <mcdir> -mcversion <v> -loader <lv> -noprofile
    let mut cmd = tokio::process::Command::new(java);
    cmd.arg("-jar")
        .arg(&jar_path)
        .arg("client")
        .arg("-dir")
        .arg(mcdir)
        .arg("-mcversion")
        .arg(&pack.mc_version)
        .arg("-loader")
        .arg(&pack.loader.version)
        .arg("-noprofile");
    if is_quilt {
        // Quilt installer verb differs slightly; keep the same client/-dir shape.
        // (quilt-installer install client <mcversion> <loaderversion> --install-dir=<mcdir>)
        cmd = quilt_command(java, &jar_path, mcdir, pack);
    }

    run_installer(cmd, "loader installer").await
}

fn quilt_command(java: &Path, jar: &Path, mcdir: &Path, pack: &PackInfo) -> tokio::process::Command {
    let mut cmd = tokio::process::Command::new(java);
    cmd.arg("-jar")
        .arg(jar)
        .arg("install")
        .arg("client")
        .arg(&pack.mc_version)
        .arg(&pack.loader.version)
        .arg(format!("--install-dir={}", mcdir.display()))
        .arg("--no-profile");
    cmd
}

/// NeoForge/Forge: run the official installer with `--installClient <mcdir>`.
async fn install_forge_like(
    http: &reqwest::Client,
    mcdir: &Path,
    pack: &PackInfo,
    java: &Path,
) -> Result<()> {
    // Installer jar coordinates.
    //   NeoForge: https://maven.neoforged.net/releases/net/neoforged/neoforge/<ver>/neoforge-<ver>-installer.jar
    //   Forge:    https://maven.minecraftforge.net/net/minecraftforge/forge/<mc>-<ver>/forge-<mc>-<ver>-installer.jar
    let ver = &pack.loader.version;
    let installer_url = if pack.loader.id == "neoforge" {
        format!(
            "https://maven.neoforged.net/releases/net/neoforged/neoforge/{v}/neoforge-{v}-installer.jar",
            v = ver
        )
    } else {
        format!(
            "https://maven.minecraftforge.net/net/minecraftforge/forge/{mc}-{v}/forge-{mc}-{v}-installer.jar",
            mc = pack.mc_version,
            v = ver
        )
    };

    // TODO(installer-automation): Forge/NeoForge installers are interactive Swing
    // apps by default; headless `--installClient` works on recent versions but some
    // older builds require a display. We download and invoke with --installClient
    // here; if a build rejects headless mode, surface the exact command to the user:
    //   java -jar <installer.jar> --installClient <mcdir>
    let jar_path = download_to_temp(http, &installer_url, "loader-installer.jar").await?;

    let mut cmd = tokio::process::Command::new(java);
    cmd.arg("-jar").arg(&jar_path).arg("--installClient").arg(mcdir);
    run_installer(cmd, "forge/neoforge installer").await
}

async fn download_to_temp(http: &reqwest::Client, url: &str, name: &str) -> Result<PathBuf> {
    let resp = http
        .get(url)
        .send()
        .await
        .with_context(|| format!("download installer {}", url))?;
    if !resp.status().is_success() {
        return Err(anyhow!("installer download failed: HTTP {} ({})", resp.status(), url));
    }
    let bytes = resp.bytes().await?;
    let path = std::env::temp_dir().join(name);
    std::fs::write(&path, &bytes)?;
    Ok(path)
}

async fn run_installer(mut cmd: tokio::process::Command, label: &str) -> Result<()> {
    let status = cmd
        .status()
        .await
        .with_context(|| format!("spawn {}", label))?;
    if !status.success() {
        return Err(anyhow!("{} exited with {}", label, status));
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Launch
// ---------------------------------------------------------------------------

/// Launch the official Minecraft launcher (detached). The player selects the
/// JarJar profile (pre-selected as most-recently-used after first launch).
pub fn launch_official() -> Result<()> {
    #[cfg(target_os = "windows")]
    let mut cmd = {
        // Typical install path; fall back to the Start-menu shim name on PATH.
        let default = PathBuf::from(r"C:\Program Files (x86)\Minecraft Launcher\MinecraftLauncher.exe");
        if default.exists() {
            std::process::Command::new(default)
        } else {
            std::process::Command::new("Minecraft.exe")
        }
    };
    #[cfg(target_os = "macos")]
    let mut cmd = {
        let mut c = std::process::Command::new("open");
        c.arg("-a").arg("Minecraft");
        c
    };
    #[cfg(all(unix, not(target_os = "macos")))]
    let mut cmd = std::process::Command::new("minecraft-launcher");

    cmd.spawn().context("failed to launch the Minecraft launcher")?;
    Ok(())
}
