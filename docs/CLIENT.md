# JarJar — Tray Client Spec

Tauri v2 app, product name **JarJar**. Targets: Windows 10+ (NSIS installer), macOS 12+
(dmg, universal), Linux (AppImage + .deb). The app is tray-resident; the window is
created on demand and **destroyed** (not hidden) on close so no webview process idles.
Idle budget: ≤ 25 MB RSS, near-zero CPU (one timer + one long-poll socket).

## 1. UX surface (complete — do not add screens)

**Tray icon** with a state badge, menu:

- `Open JarJar` (also on left-click / double-click)
- `Apply update` — only visible in state `UpdateAvailable`
- `Start Minecraft` — launches the official launcher (§6)
- `Quit`

**Window** (single page, ~380×560, resizable min 320×480):

1. **Header**: pack name, applied version → e.g. `Our Pack · v42`, connection dot
   (green/gray/red = ok/connecting/error).
2. **Update card** (only when `UpdateAvailable`/`Applying`): "v43 — Slowed down mob
   spawns…", `Apply` button with determinate progress (bytes), error text on failure
   with `Retry`.
3. **Request box**: one multiline input, placeholder *"Ask for a change… e.g. 'add a
   minimap mod'"*, `Send` button. Disabled with a tooltip while another request is
   in flight (API returns `conflict`; show whose request is running).
4. **Feed**: merged, newest-first list of (a) published versions (summary + relative
   time) and (b) requests with live status chips (`planning…`, `waiting for answer`,
   `failed: …`). A request in `awaiting_clarification` addressed to *this* player shows
   the question inline with an answer input.
5. **Settings** (gear): server URL + invite/token setup, player name (read-only after
   join), Minecraft folder override, autostart toggle, "Re-verify all files" button
   (drops the hash cache and re-syncs).

**First-run flow**: window opens automatically → fields for server URL + invite code +
player name → `POST /join` → store `config.json` → initial full sync prompt.

**Notifications** (OS-native): on `version_published` (title = pack name, body =
summary, click = open window), and on a clarification question for this player.

## 2. Client state machine

```
        ┌─────────────┐   manifest == applied   ┌────────────┐
 boot ─►│ Reconciling │────────────────────────►│  UpToDate  │◄─┐
        └─────┬───────┘                         └─────┬──────┘  │ apply ok
              │ newer version exists                  │ event: version_published
              ▼                                       ▼         │
        ┌──────────────────┐    user clicks     ┌──────────┐    │
        │ UpdateAvailable  │───────────────────►│ Applying │────┘
        └──────────────────┘       Apply        └────┬─────┘
                                                     │ error (net/hash/fs)
                                                     ▼
                                               ┌────────────┐
                                               │ ApplyError │──retry──► Applying
                                               └────────────┘
 (any state) ── connection lost ──► Offline ── reconnect ──► Reconciling
```

Apply is **always user-initiated** (no silent file swaps while the game may be running).
Exception: if `instance/` has never been populated, the first sync may run
automatically during onboarding.

## 3. Background loop (Rust, `main.rs` + `api.rs`)

- On boot and on reconnect: `GET /health`, `GET /pack/current`, reconcile (§4), then
  enter the long-poll loop `GET /events?cursor=<persisted>` with `timeout=55`.
- Long-poll errors → exponential backoff 1 s → 2 s → … → 60 s cap, state `Offline`.
- Persist the event cursor in `config.json` sibling file `cursor` (plain int in a file;
  crash-safe enough).
- Handle events: `version_published` → fetch manifest, recompute state, notify;
  `request_updated` → update feed (and notify if it's a question for this player);
  `server_status` → update header dot tooltip.

## 4. Sync & apply algorithm (`sync.rs`) — normative

Definitions: `target` = manifest of the version being applied, filtered to
`side ∈ {client, both}`; `state` = `state.json` (`schemas/client-state.schema.json`).

1. **Plan**: for each file in `target`: if `state.files[path].sha256 == target sha256`
   and the file on disk matches cached `size`+`mtime_ms` → skip. If size/mtime differ →
   re-hash the disk file; equal hash → refresh cache, skip. Else → download list.
   For each path in `state.files` **not** in `target` → delete list.
2. **Download**: for each needed hash, `GET /blobs/{sha256}` into
   `staging/<sha256>` (resume with `Range` if a partial exists). Verify SHA-256 while
   streaming; mismatch → delete partial, retry once, then `ApplyError`. Concurrency 4.
3. **Commit** (only after *all* downloads verified): for each download, rename into
   `instance/<path>` (create dirs; on Windows, if rename fails because the file is
   locked — game running — abort with the specific error "Close Minecraft first", state
   `ApplyError`, nothing partially applied thanks to per-file idempotency + re-plan on
   retry). Then process the delete list. Then write `state.json` atomically with
   `applied_version = target.version.number`.
4. **Report**: recompute state → `UpToDate`.

Files in `instance/` unknown to both `state.files` and `target` (player extras:
`options.txt`, `saves/`, `screenshots/`, extra resource packs) are never touched.
"Re-verify all files" = delete `state.files` cache (keep `applied_version = null`) and
run Reconcile, which then re-hashes everything.

Skipped-versions rule: clients always apply the **latest** version directly (manifests
are complete file sets); intermediate versions are never replayed.

## 5. Reconciling

On boot/reconnect: fetch `/pack/current`; if `current.version > state.applied_version`
→ `UpdateAvailable` (fetch that manifest for the card). If equal → `UpToDate`. Also
re-fetch the last 20 feed items (`GET /versions`, `GET /requests`) to rebuild the feed —
this covers any pruned/missed events, so event-gap recovery is automatic.

## 6. Launcher integration (`launcher.rs`) — v1 scope

Target: the **official Minecraft Launcher**. On first successful sync (and whenever
`pack.mc_version`/`loader` change):

1. Ensure the loader is installed into the launcher's game dir
   (`config.minecraft_dir` or platform default `.minecraft`):
   - **Fabric/Quilt**: download the official installer jar (URL template in code,
     version pinned by the manifest's `loader.version`), run
     `java -jar fabric-installer.jar client -dir <mcdir> -mcversion <v> -loader <lv> -noprofile`.
   - **NeoForge/Forge**: run the official installer with `--installClient <mcdir>`.
   - Requires a Java runtime: use the launcher-bundled JRE if found, else prompt with a
     download link (error state, not a crash).
2. Upsert a profile in `<mcdir>/launcher_profiles.json`: id `jarjar-<packname-slug>`,
   `"gameDir": <data>/instance`, `"lastVersionId": <loader version id string>`, icon
   set to the JarJar emerald icon (base64 PNG).
3. `Start Minecraft` menu item just launches the official launcher
   (`minecraft-launcher` / `Minecraft.exe` / `open -a Minecraft`); the player picks the
   JarJar profile (it will be pre-selected as most recently used after first launch).

Prism/MultiMC users: unsupported in v1 (documented); they can point an instance at
`<data>/instance` manually.

## 7. Implementation notes (binding)

- Tauri plugins: `tauri-plugin-single-instance`, `tauri-plugin-autostart`,
  `tauri-plugin-notification`, `tauri-plugin-dialog`. No `tauri-plugin-http` — all HTTP
  in Rust via `reqwest` (rustls). No Node runtime at runtime; UI is static assets.
- All state transitions and sync logic live in Rust; the webview only renders state it
  receives via Tauri events (`state_changed`, `progress`, `feed_updated`) and calls
  commands (`send_request`, `answer_question`, `apply_update`, `save_settings`,
  `start_minecraft`). Command + event names are part of the contract — keep exactly these.
- Hashing: `sha2` crate, streaming. JSON: `serde`. The three JSON files the client owns
  (`config.json`, `state.json`, `cursor`) are written atomically (tempfile + rename).
- The token is stored in `config.json` with file mode 0600 (Unix); OS keychain storage
  is a nice-to-have, not v1.
