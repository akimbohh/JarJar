# JarJar — Architecture

This document fixes every architectural decision. Implementers should not need to make
design choices; if something appears ambiguous, the other docs (`DATA-CONTRACTS.md`,
`API.md`, `PIPELINE.md`, `CLIENT.md`) resolve it, in that order of precedence:
**schemas/ > DATA-CONTRACTS.md > API.md / PIPELINE.md / CLIENT.md > this overview**.

---

## 1. System overview

Three deployable artifacts:

| Artifact | Runs on | Language / framework | Purpose |
|---|---|---|---|
| `jarjard` | the Linux server | Go ≥ 1.22, single static binary | Resident control daemon: HTTP API, auth, job queue, pack store, Minecraft server lifecycle. Also contains all non-resident subcommands (`worker`, `modtool`, `import`, admin verbs). |
| `claude` (external) | the Linux server | Claude Code CLI (installed separately) | Headless AI planning + config editing, spawned per job by `jarjard worker`. Not resident. |
| JarJar tray app | player PCs (Win/macOS/Linux) | Tauri v2 (Rust core + OS webview), UI in TypeScript + Vite (no framework) | Request entry, change notifications, delta sync/apply, launcher integration. |

### Request lifecycle (happy path)

1. Player types a request in the tray app → `POST /api/v1/requests`.
2. `jarjard` persists it (SQLite), enqueues a job. Queue concurrency is **1**.
3. `jarjard` spawns `jarjard worker --job <id>` (separate process).
4. Worker creates a **git worktree** of the pack repo, drops in the planning prompt +
   `CLAUDE.md`, and runs **Claude Code headless** scoped to that worktree. Claude reads
   real configs, queries mod registries only through `jarjard modtool` (a Bash-allowlisted
   helper), edits config files in place, and writes a machine-readable
   `.jarjar/plan.json`.
5. Worker **validates**: plan schema, mod versions re-verified against registry APIs,
   dependency closure, loader/MC-version compatibility, diff confined to allowed paths,
   every edited config file parses.
6. Worker **materializes**: downloads jars into the content-addressed blob store,
   verifying registry-provided hashes. (The model never downloads anything.)
7. Worker **builds** version N+1: file list + SHA-256 per file + side classification →
   `manifests/N+1.json`; commits the worktree to the pack repo; generates a player-facing
   changelog entry.
8. Worker **applies server-side**: syncs server-scoped files into the live server dir,
   restarts the Minecraft server per policy (immediate / when empty), health-checks the
   boot, rolls back on failure.
9. Daemon marks the version **published**, emits a `version_published` event; every tray
   app long-polling `/api/v1/events` lights up with the summary and an Apply button.
10. Client Apply: fetch new manifest, diff against local state, download only changed
    blobs, verify hashes, atomically swap files into the instance directory.

---

## 2. Stack decisions and justification

Decisions are final; justifications are recorded so future changes are informed.

**Daemon in Go.** Single static binary (no runtime deps on the box), ~15–30 MB RSS idle,
trivial cross-compilation, first-class HTTP + concurrency. SQLite via
`modernc.org/sqlite` (pure Go, no cgo) keeps the build static. Rust would work too, but Go
gets a smaller model to a working HTTP daemon with fewer footguns; Node/Python are
disqualified by resident memory.

**One binary, many subcommands.** `jarjard serve` is the only resident process. All heavy
or occasional work runs as `jarjard <subcommand>` child processes that exit when done
(`worker`, `import`, `modtool`, `invite`, `rollback`, `status`). This satisfies the
"nothing heavy resident" constraint and gives crash isolation: a wedged job can be killed
without touching the API server.

**Pack state = git repo + content-addressed blob store.** The pack directory (configs,
metadata, mod *references*) is a git repo — free history, diffs, worktrees for job
isolation, and rollback (`git revert`). Binary jars are **not** committed; they live in a
blob store keyed by SHA-256 and are referenced from manifests. Manifests (one JSON per
version) are the sync contract with clients.

**AI as planner+editor, code as executor.** Claude Code does what needs judgment: reading
real config files, choosing which mod fits the request, editing config values, writing the
summary. Deterministic Go code does everything that must not be guessed: resolving exact
version IDs against registries, downloading, hashing, dependency closure, packaging,
server restart. The model's output is untrusted input to a validator.

**Modrinth primary, CurseForge secondary.** Modrinth's API is open (no key), serves
SHA-512/SHA-1 hashes, machine-readable dependency and side data. CurseForge requires an
API key (config option) and lacks side metadata; supported for import and for mods absent
from Modrinth, with documented degraded metadata (side defaults to `both`).

**Tauri v2 for the tray app.** ~10 MB installers; tray-resident with **no webview
running** when the window is closed (the window is created on demand and destroyed on
close), which keeps idle RSS in the ~15–25 MB range; cross-platform tray + notifications
+ autostart plugins are first-party. Electron fails the 20 MB idle constraint outright
(~150+ MB); a fully native UI triples the client work for no user benefit.

**Long-poll instead of WebSockets.** `GET /api/v1/events?cursor=N` blocks up to 55 s.
Trivial on both ends, proxy-friendly, no persistent-connection bookkeeping in the daemon,
and latency (≤ seconds) is irrelevant for this UX.

**systemd + RCON for the Minecraft server.** The daemon never embeds or wraps the MC
server process; it controls an existing systemd unit (`systemctl start/stop/restart`,
configurable to plain commands for non-systemd setups) and talks RCON for player count and
warnings. This keeps `jarjard` restartable without touching the game server.

---

## 3. Component inventory

### 3.1 `jarjard serve` (resident daemon)

| Subsystem | Package | Responsibility |
|---|---|---|
| HTTP API | `internal/api` | REST endpoints (see `API.md`), bearer-token auth middleware, long-poll event hub, blob/manifest serving with range support. |
| Store | `internal/store` | SQLite open/migrate; typed accessors for players, invites, requests, versions, events. All writes go through here. |
| Pack | `internal/pack` | Pack git repo management (init, worktree add/remove, commit, tag), blob store (put/get/verify/GC), manifest build + load, `.mrpack`/CurseForge import. |
| Jobs | `internal/jobs` | FIFO queue (backed by the `requests` table), spawns `jarjard worker`, supervises (timeout, kill), applies approval gate. |
| MC server | `internal/mcserver` | systemd/command control, RCON client (player list, `say`), empty-server detection, boot health check, rollback of server files. |
| Config | `internal/config` | Load + validate `/etc/jarjar/jarjard.toml` (schema: `schemas/jarjard-config.schema.json`). |

### 3.2 `jarjard worker --job <id>` (per-job process)

| Stage package | Responsibility |
|---|---|
| `internal/worker` | Stage orchestration, status updates back to SQLite (single-writer discipline: worker writes only its own job row + events via `store`). |
| `internal/worker/planner` | Builds the prompt from `prompts/planner.md`, writes worktree `CLAUDE.md`, invokes `claude -p` with the exact flags in `PIPELINE.md §4`, parses result JSON, enforces turn/time limits. |
| `internal/worker/modtool` | Modrinth + CurseForge HTTP clients (search, project, version, dependencies). Doubles as the `jarjard modtool` CLI the model calls. |
| `internal/worker/validate` | Plan schema validation, registry re-verification, dependency closure, compat checks, git-diff path allowlist, config-file parse checks (TOML/JSON/JSON5/YAML/properties/SNBT-lenient). |
| `internal/worker/executor` | Deterministic apply: download jars → blob store (hash-verified), delete removed mods, apply `server_property` actions, stage files. |
| `internal/worker/changelog` | Compose the version changelog entry (player summary from the plan + deterministic file-level change list). |

### 3.3 Tray client (Tauri)

| Module | File(s) | Responsibility |
|---|---|---|
| Tray & lifecycle | `src-tauri/src/main.rs` | Tray icon + menu, autostart, single-instance, spawn/destroy window, background tick. |
| API client | `src-tauri/src/api.rs` | Typed daemon client, long-poll loop with backoff, token storage. |
| Sync engine | `src-tauri/src/sync.rs` | Manifest diff, blob download with resume, hash verification, atomic apply, local state file. |
| Launcher | `src-tauri/src/launcher.rs` | Loader installer automation + `launcher_profiles.json` profile management. |
| UI | `ui/` | One small window: request box, feed of versions/requests, update card with Apply. Vanilla TS + Vite; no UI framework. |

---

## 4. Process & memory model

Total box RAM: 10 GB. The Minecraft JVM is configured by the operator (~9 GB). Everything
else must fit in the remainder alongside the OS.

| Process | Lifetime | Expected RSS | Enforcement |
|---|---|---|---|
| `jarjard serve` | resident | 15–40 MB | systemd unit `MemoryHigh=128M`, `MemoryMax=256M` |
| `jarjard worker` | minutes per job | 50–150 MB | spawned inside a systemd transient scope |
| `claude` (child of worker) | minutes per job | 300–800 MB (Node) | same scope as worker: `systemd-run --scope -p MemoryMax=1536M -p MemorySwapMax=2G` |
| MC server JVM | resident | ~9 GB | operator-managed unit |

Rules that make this safe:

- **Queue concurrency is 1.** Never two workers, never two `claude` processes.
- Worker + claude run in one transient cgroup so a runaway model process is capped and
  killable as a unit. On OOM-kill the job fails cleanly (status `failed`, worktree removed).
- Ops recommendation (documented in install instructions, not enforced): 2 GB zram swap so
  transient worker peaks page against the mostly-idle JVM heap instead of OOMing.
- The daemon streams blobs from disk (no in-memory buffering over 1 MB per connection).

---

## 5. Repository layout (monorepo)

```
JarJar/
├── README.md
├── docs/                      # this scaffold
├── schemas/                   # JSON Schemas — single source of truth for wire formats
│   ├── manifest.schema.json
│   ├── plan.schema.json
│   ├── events.schema.json
│   ├── client-state.schema.json
│   └── jarjard-config.schema.json
├── prompts/
│   ├── CLAUDE.pack.md         # copied into each worktree as CLAUDE.md
│   └── planner.md             # prompt template for the headless run ({{placeholders}})
├── daemon/                    # Go module: github.com/akimbohh/jarjar/daemon
│   ├── go.mod
│   ├── cmd/jarjard/main.go    # subcommand dispatch: serve|worker|modtool|import|invite|rollback|status
│   └── internal/
│       ├── api/
│       ├── store/
│       ├── pack/
│       ├── jobs/
│       ├── mcserver/
│       ├── config/
│       └── worker/
│           ├── planner/
│           ├── modtool/
│           ├── validate/
│           ├── executor/
│           └── changelog/
├── client/
│   ├── src-tauri/             # Rust core (Cargo workspace member)
│   │   ├── Cargo.toml
│   │   ├── tauri.conf.json
│   │   └── src/{main.rs, api.rs, sync.rs, launcher.rs, state.rs}
│   └── ui/                    # Vite + vanilla TS
│       ├── index.html
│       ├── package.json
│       └── src/
└── .github/workflows/         # CI: go test, cargo check, release builds
```

Go module and Rust crate names, and the package boundaries above, are normative — do not
rename or merge packages.

---

## 6. On-disk layout at runtime

### Server (`jarjard`)

```
/etc/jarjar/jarjard.toml            # daemon config (schema in schemas/)
/var/lib/jarjar/
├── db.sqlite                       # store
├── secrets/claude-token            # output of `claude setup-token` (0600, owner jarjar)
├── pack/                           # git repo — the pack working copy (current version)
│   ├── jarjar.pack.json            # pack metadata (name, mc_version, loader, side_overrides)
│   ├── mods.lock.json              # mod references: platform/project/version/hash per jar
│   ├── config/…                    # mod configs (committed)
│   ├── kubejs/… defaultconfigs/…   # committed if present
│   └── .gitignore                  # ignores mods/*.jar (jars live in the blob store)
├── worktrees/job-<id>/             # transient git worktrees, removed after each job
├── blobs/sha256/<aa>/<full-hash>   # content-addressed jar/resource storage
└── manifests/<N>.json              # immutable per-version manifests
/opt/minecraft/                     # live MC server dir (operator-chosen; in config)
```

### Client (per platform data dir, e.g. `%APPDATA%/JarJar`, `~/Library/Application Support/JarJar`, `~/.local/share/jarjar`)

```
<data>/config.json                  # server URL, token, options
<data>/state.json                   # applied version, per-file hash cache (schema in schemas/)
<data>/staging/                     # in-progress downloads
<data>/instance/                    # the managed Minecraft instance (mods/, config/, …)
```

---

## 7. Security model

- **Transport:** the daemon speaks plain HTTP on `listen_addr` (default `0.0.0.0:25580`).
  Deployment docs must state the two supported production setups: (a) a WireGuard/Tailscale
  network, or (b) a reverse proxy (Caddy) terminating TLS. The daemon additionally supports
  built-in TLS via `tls_cert_file`/`tls_key_file` config keys.
- **Auth:** per-player bearer tokens (random 32 bytes, base64url), stored **hashed**
  (SHA-256) in SQLite. Roles: `player`, `admin`. Tokens are minted by redeeming
  single-use invite codes (`jarjard invite` / admin API).
- **Claude scoping:** the headless run gets tools restricted to read/edit/write/glob/grep
  plus Bash limited to `jarjard modtool …`; network tools disabled; cwd is the throwaway
  worktree; the subscription token is passed via `CLAUDE_CODE_OAUTH_TOKEN` from
  `/var/lib/jarjar/secrets/claude-token`; exact flags in `PIPELINE.md §4`. Defense in
  depth: even if the model writes outside intent, the git-diff allowlist rejects the job.
- **Blob/manifest endpoints require auth** like everything else; no anonymous downloads.

---

## 8. Non-goals for v1 (explicitly out of scope)

- Multiple packs/servers per daemon (one pack, one MC server).
- In-game chat integration, Discord bots.
- Client-side-only mods personalization per player (everyone gets `client`+`both` files).
- Automatic JarJar self-update (clients update via installers; daemon via package/binary).
- Windows/macOS server support for the daemon (Linux only).
- Resource-pack/shader management beyond files already present in an imported pack.
