//! Shared client state, on-disk formats, and path/IO helpers.
//!
//! All mutable application state lives here behind async mutexes and is owned by
//! the Rust core. The webview never mutates it directly — it only receives
//! serialized snapshots via the `state_changed` / `feed_updated` / `progress`
//! events and calls commands that mutate this state.

use std::path::{Path, PathBuf};
use std::sync::Mutex as StdMutex;

use directories::ProjectDirs;
use serde::{Deserialize, Serialize};
use tauri::AppHandle;
use tokio::sync::Mutex;

// ---------------------------------------------------------------------------
// On-disk: config.json (the client owns it; written atomically, mode 0600).
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Config {
    pub schema_version: u32,
    pub server_url: String,
    /// Bearer token; stored in this 0600 file (keychain is a post-v1 nice-to-have).
    pub token: String,
    pub player_id: String,
    pub player_name: String,
    pub server_name: String,
    #[serde(default = "default_role")]
    pub role: String,
    /// Override for the launcher game dir; `None` = platform default `.minecraft`.
    #[serde(default)]
    pub minecraft_dir: Option<String>,
    #[serde(default)]
    pub autostart: bool,
}

fn default_role() -> String {
    "player".to_string()
}

// ---------------------------------------------------------------------------
// On-disk: state.json (schemas/client-state.schema.json).
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct LocalState {
    pub schema_version: u32,
    pub applied_version: Option<u64>,
    /// path -> cached hash/size/mtime for the size+mtime skip fast-path.
    pub files: std::collections::BTreeMap<String, FileEntry>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct FileEntry {
    pub sha256: String,
    pub size: u64,
    pub mtime_ms: u64,
}

impl LocalState {
    pub fn empty() -> Self {
        LocalState {
            schema_version: 1,
            applied_version: None,
            files: Default::default(),
        }
    }
}

// ---------------------------------------------------------------------------
// Wire: manifest (schemas/manifest.schema.json) and /pack/current.
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Deserialize)]
pub struct Manifest {
    #[allow(dead_code)]
    pub schema_version: u32,
    pub pack: PackInfo,
    pub version: VersionInfo,
    pub files: Vec<ManifestFile>,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct PackInfo {
    pub name: String,
    pub mc_version: String,
    pub loader: LoaderInfo,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct LoaderInfo {
    pub id: String,
    pub version: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct VersionInfo {
    pub number: u64,
    #[allow(dead_code)]
    pub created_at: String,
    pub summary: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct ManifestFile {
    pub path: String,
    pub sha256: String,
    pub size: u64,
    /// "client" | "server" | "both"
    pub side: String,
    #[allow(dead_code)]
    pub kind: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct PackCurrent {
    pub version: u64,
    #[allow(dead_code)]
    pub summary: String,
    pub pack: PackInfo,
}

// ---------------------------------------------------------------------------
// UI-facing snapshots (serialized to the webview). Field names are a contract
// with ui/src/types.ts.
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Copy, Serialize, PartialEq, Eq)]
pub enum Phase {
    FirstRun,
    Reconciling,
    UpToDate,
    UpdateAvailable,
    Applying,
    ApplyError,
    Offline,
}

#[derive(Debug, Clone, Copy, Serialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum Connection {
    Ok,
    Connecting,
    Error,
}

#[derive(Debug, Clone, Serialize)]
pub struct InFlightRequest {
    pub player_name: String,
    pub text: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct SettingsView {
    pub server_url: String,
    pub player_name: Option<String>,
    pub minecraft_dir: Option<String>,
    pub autostart: bool,
}

#[derive(Debug, Clone, Serialize)]
pub struct UiState {
    pub phase: Phase,
    pub connection: Connection,
    pub pack_name: String,
    pub applied_version: Option<u64>,
    pub current_version: Option<u64>,
    pub update_summary: Option<String>,
    pub apply_enabled: bool,
    pub error: Option<String>,
    pub in_flight_request: Option<InFlightRequest>,
    pub min_client_notice: Option<String>,
    /// The joined player's role ("admin" or "player"); drives whether the admin
    /// setup wizard is reachable. Empty until a config is loaded.
    pub role: String,
    /// True once the server has an imported pack (a published version exists).
    /// Admins see the setup wizard while this is false.
    pub configured: bool,
    /// MC version + loader summary for the dashboard header (e.g. "1.20.1 · fabric").
    pub loader_summary: Option<String>,
    pub settings: SettingsView,
}

impl UiState {
    pub fn first_run() -> Self {
        UiState {
            phase: Phase::FirstRun,
            connection: Connection::Connecting,
            pack_name: "JarJar".into(),
            applied_version: None,
            current_version: None,
            update_summary: None,
            apply_enabled: false,
            error: None,
            in_flight_request: None,
            min_client_notice: None,
            role: String::new(),
            configured: false,
            loader_summary: None,
            settings: SettingsView {
                server_url: String::new(),
                player_name: None,
                minecraft_dir: None,
                autostart: false,
            },
        }
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct Progress {
    pub bytes_done: u64,
    pub bytes_total: u64,
    pub files_done: u64,
    pub files_total: u64,
    pub message: Option<String>,
}

/// Feed item union; `kind` is the serde tag matching ui/src/types.ts.
#[derive(Debug, Clone, Serialize)]
#[serde(tag = "kind", rename_all = "lowercase")]
pub enum FeedItem {
    Version {
        id: String,
        number: u64,
        summary: String,
        created_at: String,
    },
    Request {
        id: String,
        player_name: String,
        text: String,
        status: String,
        question: Option<String>,
        error: Option<String>,
        version: Option<u64>,
        created_at: String,
        awaiting_answer: bool,
    },
}

impl FeedItem {
    /// Sort key: newest-first by created_at string (ISO-8601 sorts lexically).
    pub fn created_at(&self) -> &str {
        match self {
            FeedItem::Version { created_at, .. } => created_at,
            FeedItem::Request { created_at, .. } => created_at,
        }
    }
}

// ---------------------------------------------------------------------------
// Paths
// ---------------------------------------------------------------------------

#[derive(Debug, Clone)]
pub struct Paths {
    pub data_dir: PathBuf,
}

impl Paths {
    pub fn resolve() -> anyhow::Result<Self> {
        // e.g. Linux ~/.local/share/JarJar, macOS ~/Library/Application Support/JarJar,
        // Windows %APPDATA%/JarJar.
        let pd = ProjectDirs::from("dev", "jarjar", "JarJar")
            .ok_or_else(|| anyhow::anyhow!("cannot resolve platform data dir"))?;
        let data_dir = pd.data_dir().to_path_buf();
        Ok(Paths { data_dir })
    }

    pub fn config_json(&self) -> PathBuf {
        self.data_dir.join("config.json")
    }
    pub fn state_json(&self) -> PathBuf {
        self.data_dir.join("state.json")
    }
    pub fn cursor(&self) -> PathBuf {
        self.data_dir.join("cursor")
    }
    pub fn instance(&self) -> PathBuf {
        self.data_dir.join("instance")
    }
    pub fn staging(&self) -> PathBuf {
        self.data_dir.join("staging")
    }
}

// ---------------------------------------------------------------------------
// Shared core
// ---------------------------------------------------------------------------

pub struct AppCore {
    pub paths: Paths,
    pub version: String,
    /// Host OS, kept for diagnostics; the User-Agent is built in `new`.
    #[allow(dead_code)]
    pub os: String,
    pub client: reqwest::Client,

    pub config: Mutex<Option<Config>>,
    pub ui: Mutex<UiState>,
    pub feed: Mutex<Vec<FeedItem>>,
    /// Cached target manifest for the currently-available update (for Apply).
    pub target: Mutex<Option<Manifest>>,
    /// Set true while an apply runs to reject concurrent applies.
    pub applying: Mutex<bool>,

    /// Handle for emitting events; set once during setup.
    app: StdMutex<Option<AppHandle>>,
    /// Last state snapshot emitted, so `emit_state` can skip byte-identical
    /// pushes (the background loop re-asserts FirstRun every 500ms, which would
    /// otherwise rebuild the webview — and wipe whatever the user is typing).
    last_state: StdMutex<Option<String>>,
    /// Tray "Apply update" menu item, toggled with state (set during setup).
    pub apply_item: StdMutex<Option<tauri::menu::MenuItem<tauri::Wry>>>,
}

impl AppCore {
    pub fn new(paths: Paths) -> anyhow::Result<Self> {
        let version = env!("CARGO_PKG_VERSION").to_string();
        let os = std::env::consts::OS.to_string();
        // Single reqwest client with the required User-Agent.
        let client = reqwest::Client::builder()
            .user_agent(format!("jarjar-client/{} ({})", version, os))
            .build()?;
        Ok(AppCore {
            paths,
            version,
            os,
            client,
            config: Mutex::new(None),
            ui: Mutex::new(UiState::first_run()),
            feed: Mutex::new(Vec::new()),
            target: Mutex::new(None),
            applying: Mutex::new(false),
            app: StdMutex::new(None),
            apply_item: StdMutex::new(None),
            last_state: StdMutex::new(None),
        })
    }

    pub fn set_app(&self, app: AppHandle) {
        *self.app.lock().unwrap() = Some(app);
    }

    pub fn app(&self) -> Option<AppHandle> {
        self.app.lock().unwrap().clone()
    }

    /// Push the current UI snapshot to the webview, skipping the emit when it is
    /// byte-identical to the last one (the reconcile loop re-asserts the same
    /// state on a timer; re-emitting would needlessly rebuild the webview).
    pub async fn emit_state(&self) {
        let snapshot = self.ui.lock().await.clone();
        let json = serde_json::to_string(&snapshot).unwrap_or_default();
        {
            let mut last = self.last_state.lock().unwrap();
            if last.as_deref() == Some(json.as_str()) {
                return;
            }
            *last = Some(json);
        }
        if let Some(app) = self.app() {
            use tauri::Emitter;
            let _ = app.emit("state_changed", &snapshot);
        }
    }

    /// Force a state emit regardless of dedupe, and refresh the dedupe cache.
    /// Used when a freshly-created webview needs the current snapshot even if it
    /// has not changed since the last emit.
    pub async fn emit_state_force(&self) {
        let snapshot = self.ui.lock().await.clone();
        *self.last_state.lock().unwrap() = Some(serde_json::to_string(&snapshot).unwrap_or_default());
        if let Some(app) = self.app() {
            use tauri::Emitter;
            let _ = app.emit("state_changed", &snapshot);
        }
    }

    pub async fn emit_feed(&self) {
        let feed = self.feed.lock().await.clone();
        if let Some(app) = self.app() {
            use tauri::Emitter;
            let _ = app.emit("feed_updated", &feed);
        }
    }

    pub fn emit_progress(&self, p: &Progress) {
        if let Some(app) = self.app() {
            use tauri::Emitter;
            let _ = app.emit("progress", p);
        }
    }
}

// ---------------------------------------------------------------------------
// Atomic IO helpers (tempfile + rename), used for config.json/state.json/cursor.
// ---------------------------------------------------------------------------

/// Write bytes atomically: temp file in the same dir, fsync-free rename over target.
/// On Unix, `mode` (e.g. 0o600) is applied to the temp file before rename.
pub fn write_atomic(path: &Path, bytes: &[u8], mode: Option<u32>) -> std::io::Result<()> {
    use std::io::Write;
    let dir = path.parent().unwrap_or_else(|| Path::new("."));
    std::fs::create_dir_all(dir)?;
    let tmp = dir.join(format!(
        ".{}.tmp.{}",
        path.file_name()
            .and_then(|s| s.to_str())
            .unwrap_or("jarjar"),
        std::process::id()
    ));
    {
        let mut f = std::fs::File::create(&tmp)?;
        #[cfg(unix)]
        if let Some(m) = mode {
            use std::os::unix::fs::PermissionsExt;
            f.set_permissions(std::fs::Permissions::from_mode(m))?;
        }
        #[cfg(not(unix))]
        let _ = mode;
        f.write_all(bytes)?;
        f.flush()?;
    }
    std::fs::rename(&tmp, path)?;
    Ok(())
}

pub fn load_config(paths: &Paths) -> Option<Config> {
    let data = std::fs::read(paths.config_json()).ok()?;
    serde_json::from_slice(&data).ok()
}

pub fn save_config(paths: &Paths, cfg: &Config) -> anyhow::Result<()> {
    let bytes = serde_json::to_vec_pretty(cfg)?;
    write_atomic(&paths.config_json(), &bytes, Some(0o600))?;
    Ok(())
}

pub fn load_state(paths: &Paths) -> LocalState {
    std::fs::read(paths.state_json())
        .ok()
        .and_then(|b| serde_json::from_slice(&b).ok())
        .unwrap_or_else(LocalState::empty)
}

pub fn save_state(paths: &Paths, st: &LocalState) -> anyhow::Result<()> {
    let bytes = serde_json::to_vec_pretty(st)?;
    write_atomic(&paths.state_json(), &bytes, None)?;
    Ok(())
}

pub fn load_cursor(paths: &Paths) -> u64 {
    std::fs::read_to_string(paths.cursor())
        .ok()
        .and_then(|s| s.trim().parse().ok())
        .unwrap_or(0)
}

pub fn save_cursor(paths: &Paths, cursor: u64) {
    // Best-effort; a lost cursor just replays a few events on next boot.
    let _ = write_atomic(&paths.cursor(), cursor.to_string().as_bytes(), None);
}
