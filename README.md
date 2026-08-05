# JarJar

**JarJar** is a self-hosted, AI-driven Minecraft modpack manager. Players type plain-English
requests ("add a cave loot mod", "slow down mob spawns") into a lightweight tray app; a
pipeline on the server turns the request into a real, validated modpack change (mods pulled
from Modrinth/CurseForge, config keys edited, pack repacked and versioned); every player's
tray app then shows a plain-English summary and a one-click **Apply** that downloads only
the delta. After applying, all clients and the server are bit-identical.

This repository currently contains the **complete technical scaffold** — architecture,
module boundaries, data contracts, config schemas, and API surfaces — written to be
implemented mechanically, without design decisions left open.

## Read in this order

| # | Document | Contents |
|---|----------|----------|
| 1 | [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | System overview, stack decisions + justification, component inventory, process & memory model, repository layout, on-disk layout, security model, non-goals |
| 2 | [`docs/DATA-CONTRACTS.md`](docs/DATA-CONTRACTS.md) | IDs, hashing, versioning, pack manifest, AI plan format, events, SQLite schema, client local state |
| 3 | [`docs/API.md`](docs/API.md) | Full HTTP API of the `jarjard` daemon (auth, endpoints, long-poll, errors) |
| 4 | [`docs/PIPELINE.md`](docs/PIPELINE.md) | The request→version pipeline: stages, Claude Code headless invocation (exact flags), validation rules, server apply/restart/rollback |
| 5 | [`docs/CLIENT.md`](docs/CLIENT.md) | Tray app spec: UX, state machine, sync/apply algorithm, launcher integration |
| 6 | [`docs/IMPLEMENTATION-PLAN.md`](docs/IMPLEMENTATION-PLAN.md) | Ordered milestones M0–M7 with acceptance criteria — **implement in this order** |

Machine-readable contracts live in [`schemas/`](schemas/) (JSON Schema, draft 2020-12).
Prompt templates for the AI planning step live in [`prompts/`](prompts/).

## Components at a glance

```
 Player PCs                                Linux server (10 GB RAM total)
┌────────────────┐   HTTPS (bearer)      ┌──────────────────────────────────────┐
│ JarJar tray    │◄────────────────────► │ jarjard (Go daemon, ~30 MB resident) │
│ (Tauri v2)     │  requests / events /  │  ├─ HTTP API + long-poll hub         │
│  instance dir  │  manifests / blobs    │  ├─ SQLite (requests, events, vers.) │
└────────────────┘                       │  ├─ pack git repo + blob store (CAS) │
                                         │  └─ job queue (concurrency = 1)      │
                                         │        │ spawns per job (transient)  │
                                         │        ▼                             │
                                         │ jarjard worker ──► claude -p (headless,
                                         │  resolver/validator/    scoped to pack
                                         │  executor/packer        worktree)    │
                                         │        │                             │
                                         │        ▼                             │
                                         │ Minecraft server (~9 GB JVM)         │
                                         │  systemd unit + RCON                 │
                                         └──────────────────────────────────────┘
```

## Hard constraints this design honors

- Everything besides the Minecraft server fits in ~1 GB: the resident daemon is a single
  Go binary (~30 MB RSS); all heavy work (including the Claude Code process) runs in
  short-lived subprocesses, one job at a time.
- The AI edit step runs **Claude Code in headless mode**, working-directory-scoped to a
  throwaway git worktree of the pack, authenticated with a Claude subscription token
  (`claude setup-token`), never an API key.
- No hallucinated mods or versions: the model can only reference mod versions it obtained
  from a real registry via the `jarjard modtool` helper CLI, and every claim is
  re-verified against the registry APIs before anything is downloaded.
- The tray app is Tauri v2: tray-resident with the window closed (~15–25 MB), small
  installers, Windows/macOS/Linux.
