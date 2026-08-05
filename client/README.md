# JarJar — Tray Client

Tray-resident Tauri v2 desktop client that keeps a Minecraft modpack instance in
sync with a JarJar server. Implements the client spec in
[`docs/CLIENT.md`](../docs/CLIENT.md) and consumes the daemon API in
[`docs/API.md`](../docs/API.md).

- **`src-tauri/`** — Rust core. Owns all state and sync logic (state machine,
  long-poll, plan/download/commit, launcher integration).
- **`ui/`** — Vite + vanilla TypeScript. A pure view: it renders snapshots pushed
  on Tauri events and calls the contract commands. No framework, no runtime Node.

## Architecture at a glance

All mutable state lives in Rust (`src-tauri/src/state.rs`). The webview never
mutates state; it only:

- **listens** to three events — `state_changed`, `progress`, `feed_updated`;
- **calls** five commands — `send_request`, `answer_question`, `apply_update`,
  `save_settings`, `start_minecraft`.

These names are a contract (CLIENT.md §7) — do not rename them.

| File | Responsibility |
|---|---|
| `src/main.rs` | Tray + menu, window create-on-demand / destroy-on-close, single-instance, background reconcile + long-poll loop, event handling + notifications, the five commands. |
| `src/api.rs` | Typed daemon client (reqwest + rustls). Error envelope parsing, `X-JarJar-Min-Client` handling, `User-Agent: jarjar-client/<ver> (<os>)`. |
| `src/sync.rs` | Normative sync/apply engine: plan (size+mtime cache → hash), staged downloads with `Range` resume + streaming SHA-256 (concurrency 4), commit-after-all-verified, atomic `state.json`. |
| `src/launcher.rs` | Official-launcher integration: loader install, `launcher_profiles.json` upsert, launch. |
| `src/state.rs` | On-disk formats (`config.json`, `state.json`, `cursor`), UI snapshot types, `AppCore` shared state, atomic IO. |

## Prerequisites

- **Rust** 1.77+ (built/tested with cargo 1.94).
- **Node** 20+ (tested with v22) for building the UI assets.
- **Tauri CLI** (only needed for `tauri dev` / bundling):
  `cargo install tauri-cli --version "^2"`.

### Linux system libraries (required to compile)

Tauri links GTK/WebKit even for `cargo check`. On Debian/Ubuntu:

```sh
sudo apt-get install -y \
  libwebkit2gtk-4.1-dev libgtk-3-dev libayatana-appindicator3-dev \
  librsvg2-dev libsoup-3.0-dev pkg-config
```

Fedora: `webkit2gtk4.1-devel gtk3-devel libappindicator-gtk3-devel librsvg2-devel libsoup3-devel`.
Windows/macOS need no extra system packages (WebView2 / WKWebView are provided by the OS).

## Develop

```sh
# 1. UI deps (once)
cd ui && npm install

# 2. Run the app (spawns the Vite dev server via beforeDevCommand, then Tauri)
cd ../src-tauri && cargo tauri dev
```

Or drive the pieces manually: `npm --prefix ui run dev` (serves on :5173, matching
`tauri.conf.json` `devUrl`) in one terminal, `cargo tauri dev` in another.

## Build

```sh
# Compile the front-end to ui/dist (embedded into the binary at build time)
npm --prefix ui run build

# Type-check only
npm --prefix ui run typecheck        # tsc --noEmit

# Rust: check / build the core
cd src-tauri
cargo check
cargo build --release

# Full installers (NSIS / dmg / AppImage / deb)
cargo tauri build
```

`cargo build` produces the binary and links successfully once the system
libraries above are installed. `cargo tauri build` additionally needs the
bundling toolchain for each target format.

## Runtime data locations

Resolved via the `directories` crate (`ProjectDirs("dev","jarjar","JarJar")`):

- Linux `~/.local/share/JarJar/`, macOS `~/Library/Application Support/JarJar/`,
  Windows `%APPDATA%\JarJar\`.
- Inside: `config.json` (mode 0600 on Unix — holds the bearer token), `state.json`
  (`schemas/client-state.schema.json`), `cursor` (persisted event seq),
  `instance/` (the managed game files), `staging/` (in-flight blob downloads).

### `config.json` shape

```jsonc
{
  "schema_version": 1,
  "server_url": "https://pack.example.com",
  "token": "<bearer token>",
  "player_id": "plr_…",
  "player_name": "alice",
  "server_name": "Our Pack",
  "role": "player",
  "minecraft_dir": null,      // launcher game-dir override; null = platform default
  "autostart": false
}
```

## Launcher support (v1)

Targets the **official Minecraft Launcher** only. On a successful apply the client
installs the loader (if needed) and upserts a `jarjar-<slug>` profile whose
`gameDir` points at `instance/`. Prism/MultiMC are unsupported in v1 — those users
can point an instance at `<data>/instance` manually.

## Known stubs / TODOs

Everything in the spec is implemented; the following are the marked soft spots:

1. **Forge/NeoForge headless install** — `src/launcher.rs`,
   `install_forge_like` (`TODO(installer-automation)`). We download the official
   installer and invoke `java -jar <installer.jar> --installClient <mcdir>`, which
   works on recent builds; older Forge installers are interactive Swing apps that
   may require a display. Fabric/Quilt installs are fully wired
   (`java -jar fabric-installer.jar client -dir … -mcversion … -loader … -noprofile`).
2. **Notification click-to-open** — `version_published` / clarification
   notifications fire natively (`src/main.rs` `notify`), but clicking them does not
   yet focus the window; the notification-plugin action wiring is left as a
   follow-up. The tray icon and `Open JarJar` open the window normally.
3. **Tray "Apply update" item** — shown always but **enabled only** in
   `UpdateAvailable`/`ApplyError` (rather than hidden) because the menu API exposes
   `set_enabled` more reliably than per-item visibility. The window's Apply button
   still appears only in those states, matching the spec's visual intent.

## Status

- `npm --prefix ui run typecheck` → passes (0 errors).
- `npm --prefix ui run build` (vite) → passes.
- `cargo check` and `cargo build` in `src-tauri/` → pass, zero warnings (with the
  Linux system libraries installed).
