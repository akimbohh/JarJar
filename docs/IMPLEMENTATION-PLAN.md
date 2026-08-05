# JarJar — Implementation Plan

Milestones are strictly ordered; each ends with concrete acceptance criteria that must
pass before the next begins. "AC" = acceptance criteria. Testing baseline for all Go
code: table-driven unit tests per package plus the listed integration checks; Rust:
`cargo test` for `sync.rs` diff/apply logic with a temp-dir fixture.

## M0 — Repo tooling

Create `daemon/go.mod` (Go ≥ 1.22, module `github.com/akimbohh/jarjar/daemon`),
`client/src-tauri` Cargo project via `create-tauri-app` (vanilla-ts template), CI
workflow running `go vet && go test ./...` and `cargo check`.

**AC:** CI green on an empty-but-compiling skeleton with the package tree from
`ARCHITECTURE.md §5` (empty files with package declarations are fine).

## M1 — Daemon core: config, store, API shell

`internal/config` (TOML load + defaults + validation per
`schemas/jarjard-config.schema.json`), `internal/store` (migrations from
`DATA-CONTRACTS.md §7`, typed accessors), `internal/api` (auth middleware, `/health`,
`/join`, invites, players admin endpoints, long-poll event hub with in-memory wakeup +
SQLite backing), `jarjard invite` CLI verb, admin token bootstrap.

**AC:** `curl` walkthrough works: start daemon → `jarjard invite` → `POST /join` →
authed `GET /health`; two parallel `GET /events` long-polls both wake on an event
inserted via a test hook; restart daemon → cursor-based catch-up returns the event.

## M2 — Pack store & import

`internal/pack`: git repo init, blob store (put/get/verify), manifest build from
tree + lock, manifest serving; `.mrpack` importer; CurseForge importer (behind
`allow_curseforge`); `GET /pack/current`, `/pack/manifests/{n}`, `/blobs/{sha}`,
`/versions`.

**AC:** `jarjard import <real .mrpack>` produces version 1: manifest validates against
`schemas/manifest.schema.json`; every file's blob exists and hash-checks; side
classification spot-checked against 3 known client-only mods; re-import is refused
(pack already initialized).

## M3 — Modtool + registry clients

`internal/worker/modtool`: Modrinth client (search/project/versions/deps, rate-limit
respect), CurseForge client, `jarjard modtool` CLI with the exact output shapes from
`PIPELINE.md §8`, compatibility filtering.

**AC:** `jarjard modtool search sodium` in a pack dir returns Sodium with correct side
info; `versions` lists only versions matching the pack's MC version + loader; unit tests
run against recorded HTTP fixtures (no live network in CI).

## M4 — Worker pipeline (the core)

`internal/jobs` (queue, spawn via systemd-run when available, plain exec fallback),
`internal/worker` stages: checkout, planner (headless invocation exactly per
`PIPELINE.md §4`), validate (V1–V9), executor, build, changelog. Requests API
(`POST /requests`, list/get, `answer`, approval endpoints) and events wiring.
Server-apply is stubbed (logs only) in this milestone.

**AC:** with a real Claude subscription token on a dev box: "add sodium" on a fresh
fabric pack yields a published version 2 whose manifest adds the Sodium jar; "make the
night brighter" against a pack containing a lighting mod yields a config-only version;
a request naming a nonexistent mod ends `infeasible`, not `failed`; a plan citing a
fake version_id (injected via a mock claude binary) is rejected by V5; kill -9 of the
worker mid-run leaves repo + DB consistent (job `failed`, worktree cleaned on daemon
restart). A `fake-claude` shell script (reads prompt, emits canned JSON) makes the
whole pipeline testable in CI without tokens.

## M5 — Server apply, restart, rollback

`internal/mcserver` (systemd/command control, RCON, empty detection, health check),
pipeline stage 6/7 for real, backup/restore, `POST /admin/rollback` + rollback worker
mode, blob GC.

**AC:** on a test box with a real (small) modded server: publish restarts per policy
(`when_empty` waits while a player is on); a deliberately broken version (corrupt jar
injected) fails health check, auto-restores, server boots on the old version, request
`failed`; rollback to N-1 produces version N+1 with identical file set to N-1 and the
server running on it.

## M6 — Tray client

Everything in `CLIENT.md`: Tauri shell, first-run join flow, long-poll loop, sync
engine, apply UX, request box + feed + clarification answers, notifications, launcher
integration, settings.

**AC:** on Windows + Linux: fresh install → join → initial sync populates `instance/`
bit-identically to the server manifest (verified by a `re-verify` pass showing zero
downloads); publishing a version on the server pops a notification within 5 s and Apply
downloads only the changed files (byte counter sanity-checked); killing the app mid-
download and re-applying resumes and finishes; official launcher shows the JarJar
profile and boots the pack; idle RSS with window closed ≤ 25 MB.

## M7 — Hardening & release

TLS config keys, rate limiting, `X-JarJar-Min-Client` gate, structured logging
(`log/slog`, JSON), `jarjard status`, systemd unit files + install docs (including
`claude setup-token` walkthrough and zram recommendation), GitHub release workflow
building `jarjard` (linux/amd64, linux/arm64) and client installers (NSIS, dmg,
AppImage, deb).

**AC:** one-page install doc executed start-to-finish on a clean VM by following it
literally; end-to-end demo: import existing pack → friend joins via invite → friend
requests a mod → everyone applies → all clients + server bit-identical (hash audit
script included in `daemon/testdata/`).

---

## Standing constraints for whoever implements this

- Never widen the Claude Code tool allowlist or path allowlist without updating
  `PIPELINE.md` first — the validator, not the model, is the security boundary.
- Any change to a wire format starts in `schemas/` and `DATA-CONTRACTS.md`, then code.
- Keep the daemon dependency-light: stdlib + `modernc.org/sqlite` + a TOML parser +
  a JSON Schema validator; no web frameworks, no ORM.
- Do not add features not in these docs (no Discord, no multi-pack, no client mod
  personalization) — file them as issues instead.
