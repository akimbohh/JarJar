// Prevent an extra console window on Windows release builds.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

//! JarJar tray client entry point: tray + menu, window lifecycle
//! (create-on-demand / destroy-on-close), the background reconcile + long-poll
//! loop, event handling with OS notifications, and the five contract commands.

mod api;
mod launcher;
mod state;
mod sync;

use std::sync::Arc;
use std::time::Duration;

use serde::Deserialize;
use tauri::menu::{MenuBuilder, MenuItem, MenuItemBuilder};
use tauri::tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent};
use tauri::{
    AppHandle, Manager, RunEvent, State, WebviewUrl, WebviewWindowBuilder, Wry,
};
use tauri_plugin_autostart::{ManagerExt, MacosLauncher};
use tauri_plugin_dialog::{DialogExt, MessageDialogButtons};
use tauri_plugin_notification::NotificationExt;
use tauri_plugin_updater::UpdaterExt;

use api::DaemonClient;
use state::{
    AppCore, Config, Connection, FeedItem, Manifest, Paths, Phase, SettingsView,
};

type SharedCore = Arc<AppCore>;

const NON_TERMINAL: &[&str] = &[
    "queued",
    "planning",
    "awaiting_clarification",
    "awaiting_approval",
    "materializing",
    "applying_server",
];

fn main() {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "info".into()),
        )
        .init();

    let paths = Paths::resolve().expect("resolve data dir");
    std::fs::create_dir_all(&paths.data_dir).ok();
    let core: SharedCore = Arc::new(AppCore::new(paths).expect("build core"));

    // Load any existing config so the loop can start reconciling immediately.
    if let Some(cfg) = state::load_config(&core.paths) {
        // Seed the UI snapshot with what we already know.
        tauri::async_runtime::block_on(async {
            let mut ui = core.ui.lock().await;
            ui.phase = Phase::Reconciling;
            ui.pack_name = cfg.server_name.clone();
            ui.settings = SettingsView {
                server_url: cfg.server_url.clone(),
                player_name: Some(cfg.player_name.clone()),
                minecraft_dir: cfg.minecraft_dir.clone(),
                autostart: cfg.autostart,
            };
            *core.config.lock().await = Some(cfg);
        });
    }

    let core_for_setup = core.clone();

    tauri::Builder::default()
        // Single instance: a second launch just opens the window here.
        .plugin(tauri_plugin_single_instance::init(|app, _argv, _cwd| {
            open_window(app);
        }))
        .plugin(tauri_plugin_autostart::init(
            MacosLauncher::LaunchAgent,
            None,
        ))
        .plugin(tauri_plugin_notification::init())
        .plugin(tauri_plugin_dialog::init())
        // Self-updater: pulls signed release artifacts from GitHub (see
        // tauri.conf.json `plugins.updater`). Paired with the process plugin,
        // which provides the relaunch (`app.restart()`) after an install.
        .plugin(tauri_plugin_updater::Builder::new().build())
        .plugin(tauri_plugin_process::init())
        .manage(core.clone())
        .invoke_handler(tauri::generate_handler![
            send_request,
            answer_question,
            apply_update,
            save_settings,
            start_minecraft
        ])
        .setup(move |app| {
            let core = core_for_setup;
            core.set_app(app.handle().clone());

            // --- Tray menu ---
            let open_i = MenuItemBuilder::with_id("open", "Open JarJar").build(app)?;
            let apply_i = MenuItemBuilder::with_id("apply", "Apply update")
                .enabled(false)
                .build(app)?;
            let start_i = MenuItemBuilder::with_id("start", "Start Minecraft").build(app)?;
            let check_updates_i =
                MenuItemBuilder::with_id("check_updates", "Check for updates").build(app)?;
            let quit_i = MenuItemBuilder::with_id("quit", "Quit").build(app)?;
            let menu = MenuBuilder::new(app)
                .items(&[&open_i, &apply_i, &start_i])
                .separator()
                .item(&check_updates_i)
                .item(&quit_i)
                .build()?;

            // Keep a handle to toggle "Apply update" visibility with state.
            core.set_apply_item(apply_i.clone());

            let icon = app
                .default_window_icon()
                .cloned()
                .expect("bundled default icon");
            let _tray = TrayIconBuilder::with_id("main")
                .icon(icon)
                .tooltip("JarJar")
                .menu(&menu)
                .show_menu_on_left_click(false)
                .on_menu_event(|app, event| {
                    let core: SharedCore = app.state::<SharedCore>().inner().clone();
                    match event.id().as_ref() {
                        "open" => open_window(app),
                        "apply" => {
                            tauri::async_runtime::spawn(async move {
                                perform_apply(&core).await;
                            });
                        }
                        "start" => {
                            if let Err(e) = start_launcher(&core) {
                                notify(app, "JarJar", &format!("Couldn't start Minecraft: {}", e));
                            }
                        }
                        "check_updates" => {
                            // Manual check: report both "up to date" and errors.
                            let handle = app.clone();
                            tauri::async_runtime::spawn(async move {
                                run_update_check(handle, true).await;
                            });
                        }
                        "quit" => app.exit(0),
                        _ => {}
                    }
                })
                .on_tray_icon_event(|tray, event| {
                    if let TrayIconEvent::Click {
                        button: MouseButton::Left,
                        button_state: MouseButtonState::Up,
                        ..
                    } = event
                    {
                        open_window(tray.app_handle());
                    }
                })
                .build(app)?;

            // First run opens the window automatically for the join flow.
            let has_config = tauri::async_runtime::block_on(core.config.lock()).is_some();
            if !has_config {
                open_window(app.handle());
            }

            // Kick off the background reconcile + long-poll loop.
            let loop_core = core.clone();
            tauri::async_runtime::spawn(async move {
                background_loop(loop_core).await;
            });

            // Check for an app update once, shortly after startup. Silent on
            // "no update available"; failures (e.g. offline) are logged only so
            // they never disturb the rest of the app.
            let update_handle = app.handle().clone();
            tauri::async_runtime::spawn(async move {
                tokio::time::sleep(Duration::from_secs(5)).await;
                run_update_check(update_handle, false).await;
            });

            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("build tauri app")
        .run(|_app, event| {
            // Tray-resident: closing the window destroys the webview but must not
            // quit the app. Only the Quit menu item exits (via app.exit).
            if let RunEvent::ExitRequested { api, .. } = event {
                api.prevent_exit();
            }
        });
}

// ---------------------------------------------------------------------------
// Window lifecycle
// ---------------------------------------------------------------------------

/// Create the window on demand (or focus it if it already exists). The window is
/// destroyed on close by default (we never call hide), so no webview idles.
fn open_window(app: &AppHandle) {
    if let Some(w) = app.get_webview_window("main") {
        let _ = w.show();
        let _ = w.set_focus();
        return;
    }
    let res = WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
        .title("JarJar")
        .inner_size(380.0, 560.0)
        .min_inner_size(320.0, 480.0)
        .resizable(true)
        .build();
    if let Err(e) = res {
        tracing::error!("failed to create window: {}", e);
        return;
    }
    // The freshly-loaded webview registers its listeners asynchronously; re-emit
    // the current snapshot a few times so it catches up without an extra command.
    let core: SharedCore = app.state::<SharedCore>().inner().clone();
    tauri::async_runtime::spawn(async move {
        for delay in [120u64, 350, 800] {
            tokio::time::sleep(Duration::from_millis(delay)).await;
            core.emit_state().await;
            core.emit_feed().await;
        }
    });
}

fn notify(app: &AppHandle, title: &str, body: &str) {
    let _ = app
        .notification()
        .builder()
        .title(title)
        .body(body)
        .show();
}

// ---------------------------------------------------------------------------
// Self-updater (Tauri v2 updater plugin)
// ---------------------------------------------------------------------------

/// Check GitHub for a newer app release and, if one exists, download + install
/// it and relaunch. Runs entirely off the UI thread (called from a spawned
/// task) so a slow or failing check never blocks the app.
///
/// `manual` distinguishes the tray-triggered check from the silent startup one:
/// - update found: always installs, shows a dialog, then restarts;
/// - no update: only the manual check tells the user "you're on the latest";
/// - error: logged always, surfaced in a dialog only on a manual check.
async fn run_update_check(app: AppHandle, manual: bool) {
    // `check()` needs the updater; if the plugin can't build (misconfig) we
    // treat it as a soft error rather than panicking.
    let updater = match app.updater() {
        Ok(u) => u,
        Err(e) => {
            tracing::warn!("updater unavailable: {}", e);
            if manual {
                app.dialog()
                    .message(format!("Couldn't check for updates: {}", e))
                    .title("JarJar")
                    .buttons(MessageDialogButtons::Ok)
                    .blocking_show();
            }
            return;
        }
    };

    match updater.check().await {
        Ok(Some(update)) => {
            // An update is available: download + install (progress callbacks are
            // unused here — the tray flow is intentionally minimal).
            match update
                .download_and_install(|_chunk, _total| {}, || {})
                .await
            {
                Ok(()) => {
                    let version = update.version.clone();
                    app.dialog()
                        .message(format!(
                            "JarJar was updated to {}. The app will now restart.",
                            version
                        ))
                        .title("Update installed")
                        .buttons(MessageDialogButtons::Ok)
                        .blocking_show();
                    // Relaunch into the freshly installed version.
                    app.restart();
                }
                Err(e) => {
                    tracing::warn!("update install failed: {}", e);
                    if manual {
                        app.dialog()
                            .message(format!("Couldn't install the update: {}", e))
                            .title("JarJar")
                            .buttons(MessageDialogButtons::Ok)
                            .blocking_show();
                    }
                }
            }
        }
        Ok(None) => {
            // Already current. Stay silent unless the user asked explicitly.
            if manual {
                app.dialog()
                    .message("You're on the latest version.")
                    .title("JarJar")
                    .buttons(MessageDialogButtons::Ok)
                    .blocking_show();
            }
        }
        Err(e) => {
            tracing::warn!("update check failed: {}", e);
            if manual {
                app.dialog()
                    .message(format!("Couldn't check for updates: {}", e))
                    .title("JarJar")
                    .buttons(MessageDialogButtons::Ok)
                    .blocking_show();
            }
        }
    }
}

// ---------------------------------------------------------------------------
// State + tray helpers
// ---------------------------------------------------------------------------

/// Push UI state and reflect the phase in the tray "Apply update" item.
async fn push_state(core: &AppCore) {
    core.emit_state().await;
    let phase = core.ui.lock().await.phase;
    let show_apply = phase == Phase::UpdateAvailable || phase == Phase::ApplyError;
    core.set_apply_enabled(show_apply);
}

fn client_for(core: &AppCore, cfg: &Config) -> DaemonClient {
    DaemonClient::new(
        core.client.clone(),
        &cfg.server_url,
        Some(cfg.token.clone()),
        &core.version,
    )
}

async fn cfg_snapshot(core: &AppCore) -> Option<Config> {
    core.config.lock().await.clone()
}

fn is_terminal(status: &str) -> bool {
    !NON_TERMINAL.contains(&status)
}

// ---------------------------------------------------------------------------
// Background loop: reconcile then long-poll (CLIENT.md §3, §5)
// ---------------------------------------------------------------------------

async fn background_loop(core: SharedCore) {
    loop {
        // Wait until the user has joined.
        let Some(cfg) = cfg_snapshot(&core).await else {
            set_phase(&core, Phase::FirstRun, Connection::Connecting).await;
            tokio::time::sleep(Duration::from_millis(500)).await;
            continue;
        };

        // Reconcile on boot/reconnect.
        if let Err(e) = reconcile(&core, &cfg).await {
            tracing::warn!("reconcile failed: {}", e);
            set_connection(&core, Connection::Error).await;
            set_phase(&core, Phase::Offline, Connection::Error).await;
            tokio::time::sleep(Duration::from_secs(3)).await;
            continue;
        }

        // Long-poll loop with exponential backoff on error.
        let client = client_for(&core, &cfg);
        let mut backoff = 1u64;
        loop {
            let cursor = state::load_cursor(&core.paths);
            match client.events(cursor, 55).await {
                Ok(resp) => {
                    backoff = 1;
                    set_connection(&core, Connection::Ok).await;
                    state::save_cursor(&core.paths, resp.cursor);
                    for ev in &resp.events {
                        handle_event(&core, &cfg, ev).await;
                    }
                    // Surface any min-client requirement observed on the response.
                    apply_min_client(&core, &client).await;
                }
                Err(e) => {
                    tracing::warn!("events poll error: {}", e);
                    set_phase(&core, Phase::Offline, Connection::Error).await;
                    tokio::time::sleep(Duration::from_secs(backoff)).await;
                    backoff = (backoff * 2).min(60);
                    // On reconnect, re-reconcile to catch anything missed.
                    if reconcile(&core, &cfg).await.is_ok() {
                        set_connection(&core, Connection::Ok).await;
                    }
                }
            }
        }
    }
}

async fn set_phase(core: &AppCore, phase: Phase, conn: Connection) {
    {
        let mut ui = core.ui.lock().await;
        ui.phase = phase;
        ui.connection = conn;
    }
    push_state(core).await;
}

async fn set_connection(core: &AppCore, conn: Connection) {
    {
        let mut ui = core.ui.lock().await;
        ui.connection = conn;
    }
    push_state(core).await;
}

async fn apply_min_client(core: &AppCore, client: &DaemonClient) {
    let notice = client.min_client_notice();
    let mut ui = core.ui.lock().await;
    if ui.min_client_notice != notice {
        ui.min_client_notice = notice;
        if ui.min_client_notice.is_some() {
            // A too-old client must not apply (§ API compatibility).
            ui.apply_enabled = false;
        }
        drop(ui);
        core.emit_state().await;
    }
}

/// Reconcile local state against `/pack/current` and rebuild the feed (§5).
async fn reconcile(core: &AppCore, cfg: &Config) -> anyhow::Result<()> {
    let client = client_for(core, cfg);

    // Health first (also validates the URL/token path).
    let health = client.health().await?;
    let _ = health.status;

    let current = client.pack_current().await?;
    let local = state::load_state(&core.paths);
    let applied = local.applied_version;

    // Determine phase.
    let mut target: Option<Manifest> = None;
    let phase;
    let mut update_summary = None;
    if current.version == 0 {
        // Nothing imported yet.
        phase = Phase::UpToDate;
    } else if applied.map(|a| a >= current.version).unwrap_or(false) {
        phase = Phase::UpToDate;
    } else {
        // A newer version exists — fetch its manifest for the update card.
        let m = client.manifest(current.version).await?;
        update_summary = Some(m.version.summary.clone());
        target = Some(m);
        phase = Phase::UpdateAvailable;
    }

    // Rebuild the feed from the last 20 versions + requests (event-gap recovery).
    let versions = client.list_versions(20).await.unwrap_or_default();
    let requests = client.list_requests(20).await.unwrap_or_default();
    let mut feed: Vec<FeedItem> = Vec::new();
    for v in versions {
        feed.push(FeedItem::Version {
            id: format!("v{}", v.number),
            number: v.number,
            summary: v.summary,
            created_at: v.created_at,
        });
    }
    let mut in_flight = None;
    for r in requests {
        if in_flight.is_none() && !is_terminal(&r.status) {
            in_flight = Some(state::InFlightRequest {
                player_name: r.player_name.clone(),
                text: r.text.clone(),
            });
        }
        let awaiting = r.status == "awaiting_clarification"
            && r.question.is_some()
            && r.player_name == cfg.player_name;
        feed.push(FeedItem::Request {
            id: r.id,
            player_name: r.player_name,
            text: r.text,
            status: r.status,
            question: r.question,
            error: r.error,
            version: r.version,
            created_at: r.created_at,
            awaiting_answer: awaiting,
        });
    }
    feed.sort_by(|a, b| b.created_at().cmp(a.created_at()));

    // Commit the snapshot.
    let notice = client.min_client_notice();
    {
        let mut ui = core.ui.lock().await;
        ui.phase = phase;
        ui.connection = Connection::Ok;
        ui.pack_name = current.pack.name.clone();
        ui.applied_version = applied;
        ui.current_version = if current.version == 0 {
            None
        } else {
            Some(current.version)
        };
        ui.update_summary = update_summary;
        ui.in_flight_request = in_flight;
        ui.min_client_notice = notice.clone();
        ui.apply_enabled = phase == Phase::UpdateAvailable && notice.is_none();
    }
    *core.feed.lock().await = feed;
    *core.target.lock().await = target;

    push_state(core).await;
    core.emit_feed().await;

    // Onboarding auto-sync: if the instance has never been populated and there is
    // content to apply, run the first sync automatically (CLIENT.md §2 exception).
    if applied.is_none() && current.version > 0 && instance_is_empty(&core.paths) {
        perform_apply(core).await;
    }

    Ok(())
}

fn instance_is_empty(paths: &Paths) -> bool {
    match std::fs::read_dir(paths.instance()) {
        Ok(mut rd) => rd.next().is_none(),
        Err(_) => true,
    }
}

/// Handle one long-poll event: notify, then refresh state/feed.
async fn handle_event(core: &AppCore, cfg: &Config, ev: &api::Event) {
    let app = core.app();
    match ev.kind.as_str() {
        "version_published" => {
            #[derive(Deserialize)]
            struct P {
                #[allow(dead_code)]
                version: u64,
                summary: String,
            }
            if let (Some(app), Ok(p)) =
                (app.as_ref(), serde_json::from_value::<P>(ev.payload.clone()))
            {
                let title = core.ui.lock().await.pack_name.clone();
                notify(app, &title, &p.summary);
            }
            // Recompute state (fetch new manifest, rebuild feed).
            let _ = reconcile(core, cfg).await;
        }
        "request_updated" => {
            #[derive(Deserialize)]
            struct P {
                #[allow(dead_code)]
                request_id: String,
                status: String,
                player_name: String,
                #[allow(dead_code)]
                text: String,
                question: Option<String>,
            }
            if let Ok(p) = serde_json::from_value::<P>(ev.payload.clone()) {
                // Notify on a clarification question addressed to this player.
                if p.status == "awaiting_clarification"
                    && p.player_name == cfg.player_name
                    && p.question.is_some()
                {
                    if let Some(app) = app.as_ref() {
                        notify(
                            app,
                            "JarJar needs your input",
                            p.question.as_deref().unwrap_or("A question is waiting."),
                        );
                    }
                }
            }
            let _ = reconcile(core, cfg).await;
        }
        "server_status" => {
            // MC server state; informational for the header tooltip. We keep the
            // daemon-connection dot as-is and just log it.
            tracing::debug!("server_status: {:?}", ev.payload);
        }
        other => tracing::debug!("unknown event type {}", other),
    }
}

// ---------------------------------------------------------------------------
// Apply (user-initiated)
// ---------------------------------------------------------------------------

async fn perform_apply(core: &AppCore) {
    // Guard against concurrent applies.
    {
        let mut a = core.applying.lock().await;
        if *a {
            return;
        }
        *a = true;
    }

    let result = perform_apply_inner(core).await;

    match result {
        Ok(version) => {
            {
                let mut ui = core.ui.lock().await;
                ui.applied_version = Some(version);
                ui.phase = Phase::UpToDate;
                ui.update_summary = None;
                ui.error = None;
                ui.apply_enabled = false;
            }
            push_state(core).await;
            // Ensure the loader is installed and the launcher profile points here.
            ensure_launcher(core).await;
        }
        Err(e) => {
            {
                let mut ui = core.ui.lock().await;
                ui.phase = Phase::ApplyError;
                ui.error = Some(e.to_string());
            }
            push_state(core).await;
        }
    }

    *core.applying.lock().await = false;
}

async fn perform_apply_inner(core: &AppCore) -> anyhow::Result<u64> {
    let cfg = cfg_snapshot(core)
        .await
        .ok_or_else(|| anyhow::anyhow!("not joined"))?;
    let client = client_for(core, &cfg);

    // Resolve the target manifest (use the cached one or fetch current).
    let target = {
        let cached = core.target.lock().await.clone();
        match cached {
            Some(m) => m,
            None => {
                let current = client.pack_current().await?;
                if current.version == 0 {
                    anyhow::bail!("no version to apply");
                }
                client.manifest(current.version).await?
            }
        }
    };

    {
        let mut ui = core.ui.lock().await;
        ui.phase = Phase::Applying;
        ui.current_version = Some(target.version.number);
        ui.error = None;
    }
    push_state(core).await;

    sync::apply(core, &client, &target).await
}

/// Install/verify the loader and upsert the launcher profile (best-effort).
async fn ensure_launcher(core: &AppCore) {
    let Some(cfg) = cfg_snapshot(core).await else {
        return;
    };
    // We need the pack info; take it from the current target or /pack/current.
    let pack = {
        let t = core.target.lock().await.clone();
        match t {
            Some(m) => m.pack,
            None => match client_for(core, &cfg).pack_current().await {
                Ok(c) => c.pack,
                Err(_) => return,
            },
        }
    };
    if let Err(e) = launcher::ensure_loader(
        &core.client,
        &core.paths,
        &pack,
        &cfg.minecraft_dir,
    )
    .await
    {
        tracing::warn!("launcher setup: {}", e);
        if let Some(app) = core.app() {
            notify(&app, "JarJar — launcher setup", &e.to_string());
        }
    }
}

fn start_launcher(_core: &AppCore) -> anyhow::Result<()> {
    launcher::launch_official()
}

// ---------------------------------------------------------------------------
// Commands (the five contract commands)
// ---------------------------------------------------------------------------

#[tauri::command]
async fn send_request(text: String, state: State<'_, SharedCore>) -> Result<(), String> {
    let core = state.inner().clone();
    let cfg = cfg_snapshot(&core).await.ok_or("Not joined yet")?;
    let client = client_for(&core, &cfg);
    match client.post_request(&text).await {
        Ok(_req) => {
            // Refresh the feed so the new request shows immediately.
            let _ = reconcile(&core, &cfg).await;
            Ok(())
        }
        Err(e) if e.is_conflict() => Err(e.to_string()),
        Err(e) => Err(e.to_string()),
    }
}

#[tauri::command]
async fn answer_question(
    request_id: String,
    text: String,
    state: State<'_, SharedCore>,
) -> Result<(), String> {
    let core = state.inner().clone();
    let cfg = cfg_snapshot(&core).await.ok_or("Not joined yet")?;
    let client = client_for(&core, &cfg);
    client
        .answer(&request_id, &text)
        .await
        .map_err(|e| e.to_string())?;
    let _ = reconcile(&core, &cfg).await;
    Ok(())
}

#[tauri::command]
async fn apply_update(state: State<'_, SharedCore>) -> Result<(), String> {
    let core = state.inner().clone();
    perform_apply(&core).await;
    Ok(())
}

/// Settings payload; also carries the first-run join fields and the reverify flag.
#[derive(Debug, Deserialize)]
struct SettingsPayload {
    server_url: Option<String>,
    invite_code: Option<String>,
    player_name: Option<String>,
    minecraft_dir: Option<String>,
    autostart: Option<bool>,
    reverify: Option<bool>,
}

#[tauri::command]
async fn save_settings(
    settings: SettingsPayload,
    app: AppHandle,
    state: State<'_, SharedCore>,
) -> Result<(), String> {
    let core = state.inner().clone();

    // Re-verify all files: drop the hash cache and re-reconcile (§4).
    if settings.reverify.unwrap_or(false) {
        let st = state::LocalState {
            schema_version: 1,
            applied_version: None,
            files: Default::default(),
        };
        state::save_state(&core.paths, &st).map_err(|e| e.to_string())?;
        if let Some(cfg) = cfg_snapshot(&core).await {
            let _ = reconcile(&core, &cfg).await;
        }
        return Ok(());
    }

    // First-run join: invite_code present and no config yet.
    if settings.invite_code.is_some() && cfg_snapshot(&core).await.is_none() {
        let server_url = settings
            .server_url
            .clone()
            .ok_or("Server URL is required")?;
        let invite = settings.invite_code.clone().unwrap();
        let name = settings.player_name.clone().ok_or("Player name is required")?;

        let tmp = DaemonClient::new(core.client.clone(), &server_url, None, &core.version);
        let join = tmp.join(&invite, &name).await.map_err(|e| e.to_string())?;

        let cfg = Config {
            schema_version: 1,
            server_url,
            token: join.token,
            player_id: join.player_id,
            player_name: name.clone(),
            server_name: join.server_name.clone(),
            role: if join.role.is_empty() {
                "player".into()
            } else {
                join.role
            },
            minecraft_dir: None,
            autostart: false,
        };
        state::save_config(&core.paths, &cfg).map_err(|e| e.to_string())?;
        {
            let mut ui = core.ui.lock().await;
            ui.phase = Phase::Reconciling;
            ui.pack_name = cfg.server_name.clone();
            ui.settings = SettingsView {
                server_url: cfg.server_url.clone(),
                player_name: Some(cfg.player_name.clone()),
                minecraft_dir: None,
                autostart: false,
            };
        }
        *core.config.lock().await = Some(cfg.clone());
        push_state(&core).await;

        // Reconcile now (also triggers onboarding auto-sync if needed).
        let core2 = core.clone();
        tauri::async_runtime::spawn(async move {
            let _ = reconcile(&core2, &cfg).await;
        });
        return Ok(());
    }

    // Regular settings update.
    let mut cfg = cfg_snapshot(&core).await.ok_or("Not joined yet")?;
    if let Some(url) = settings.server_url {
        if !url.trim().is_empty() {
            cfg.server_url = url;
        }
    }
    cfg.minecraft_dir = settings
        .minecraft_dir
        .filter(|s| !s.trim().is_empty());
    if let Some(auto) = settings.autostart {
        cfg.autostart = auto;
        // Reflect the toggle in the OS autostart registration.
        let mgr = app.autolaunch();
        let _ = if auto { mgr.enable() } else { mgr.disable() };
    }
    state::save_config(&core.paths, &cfg).map_err(|e| e.to_string())?;
    {
        let mut ui = core.ui.lock().await;
        ui.settings = SettingsView {
            server_url: cfg.server_url.clone(),
            player_name: Some(cfg.player_name.clone()),
            minecraft_dir: cfg.minecraft_dir.clone(),
            autostart: cfg.autostart,
        };
    }
    *core.config.lock().await = Some(cfg);
    push_state(&core).await;
    Ok(())
}

#[tauri::command]
async fn start_minecraft(state: State<'_, SharedCore>) -> Result<(), String> {
    let core = state.inner().clone();
    start_launcher(&core).map_err(|e| e.to_string())
}

// ---------------------------------------------------------------------------
// Tray "Apply update" toggle storage lives on AppCore (see below).
// ---------------------------------------------------------------------------

// Extension methods for the tray menu item, kept here to avoid leaking Tauri
// types into state.rs.
impl AppCore {
    pub fn set_apply_item(&self, item: MenuItem<Wry>) {
        *self.apply_item.lock().unwrap() = Some(item);
    }
    pub fn set_apply_enabled(&self, enabled: bool) {
        if let Some(item) = self.apply_item.lock().unwrap().as_ref() {
            let _ = item.set_enabled(enabled);
        }
    }
}
