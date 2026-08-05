# JarJar — Request Pipeline

How a request (`req_…`) becomes a published pack version. Runs inside
`jarjard worker --job <id>`, spawned by the daemon as
`systemd-run --scope -p MemoryMax=<claude.memory_max> -p MemorySwapMax=2G -- jarjard worker --job <id>`.
One worker at a time, enforced by the daemon's queue. Every stage updates
`requests.status` and emits a `request_updated` event on transition.

Stages: **checkout → plan (Claude Code) → validate → materialize → build → server-apply
→ publish**. Any failure before *publish* leaves the pack repo and live server exactly
as they were.

---

## 1. Checkout

1. `git -C /var/lib/jarjar/pack worktree add /var/lib/jarjar/worktrees/job-<id> HEAD`.
2. Write `CLAUDE.md` into the worktree root — the rendered contents of
   `prompts/CLAUDE.pack.md` (placeholders filled from `jarjar.pack.json`).
3. Create `<worktree>/.jarjar/` and copy in a **read-only snapshot** of the live
   `server.properties` as `.jarjar/server.properties` (informational; the model changes
   server properties only via `server_property` plan actions, never by editing a file).
4. The worktree therefore contains: `jarjar.pack.json`, `mods.lock.json` (the mod
   inventory the model reads), `config/`, `defaultconfigs/`, `kubejs/`, `scripts/`,
   `CLAUDE.md`, `.jarjar/`. Jar binaries are **not** present (gitignored).

Cleanup (always, success or failure): `git worktree remove --force` + delete the dir.

## 2. Plan — Claude Code headless

### Prompt

Rendered from `prompts/planner.md`. Placeholders: `{{request_text}}`,
`{{player_name}}`, `{{pack_name}}`, `{{mc_version}}`, `{{loader_id}}`,
`{{loader_version}}`, `{{clarification_qa}}` (empty, or the prior
question/answer pairs when re-planning after a clarification).

### Invocation (exact)

Working directory: the worktree. Environment is **sanitized**: start from a minimal env
(`PATH`, `TERM`), explicitly *unset* `ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN`
(they would take precedence over the subscription token), and set:

| Env var | Value |
|---|---|
| `HOME` | `/var/lib/jarjar/claude-home` (dedicated, initially empty) |
| `CLAUDE_CONFIG_DIR` | `/var/lib/jarjar/claude-config` (isolates from any user-level `~/.claude`) |
| `CLAUDE_CODE_OAUTH_TOKEN` | contents of `claude.token_file` (from `claude setup-token`; long-lived subscription token, ~1-year expiry — the daemon logs a warning when auth fails so the operator re-runs setup-token) |

```sh
claude -p "<rendered prompt>" \
  --output-format json \
  --json-schema "$(cat schemas/plan.schema.json)" \
  --model <claude.model> \
  --max-turns <claude.max_turns> \
  --allowedTools "Read,Edit,Write,Glob,Grep,Bash(jarjard modtool *)" \
  --disallowedTools "WebFetch,WebSearch,Task,NotebookEdit" \
  --permission-mode acceptEdits \
  --mcp-config '{"mcpServers":{}}' \
  --settings '{"permissions":{"deny":["Read(//var/lib/jarjar/secrets/**)","Edit(//var/lib/jarjar/secrets/**)","Read(~/.ssh/**)"]}}'
```

Rationale for each flag (do not drop any):

- `-p --output-format json` — one-shot headless run; result is a single JSON object on
  stdout with fields `result`, `structured_output`, `num_turns`, `stop_reason`,
  `total_cost_usd`, `usage`, `session_id`. The worker logs `total_cost_usd` and `usage`
  per job.
- `--json-schema` — forces the final answer to validate against
  `schemas/plan.schema.json`; the worker reads the plan from `structured_output`
  (and still re-validates it itself — never trust a single validator).
- `--allowedTools` — file tools plus Bash restricted to the prefix
  `jarjard modtool *`. This is the model's **only** window to the outside world: real
  registry data in, nothing else. The pattern syntax (`Bash(jarjard modtool *)`,
  space-before-`*` = word boundary) is Claude Code's documented allowlist format.
- `--disallowedTools` — belt-and-braces removal of network and subagent tools.
- `--permission-mode acceptEdits` — auto-approves file edits and mundane filesystem
  commands so the run never blocks on an interactive prompt; anything not allowlisted
  simply fails, it does not hang.
- `--mcp-config '{"mcpServers":{}}'` — an explicit (empty) MCP config means *only*
  these servers load: none. Prevents any host-level `.mcp.json` from leaking in.
- `--settings` — inline deny rules keeping the token file and host keys unreadable even
  inside this sandbox. (Note: `--bare` is deliberately **not** used — bare mode skips
  discovery but also does not read `CLAUDE_CODE_OAUTH_TOKEN`.)

Supervision: hard timeout `claude.timeout_minutes` (kill the scope), treat exit-code ≠ 0,
missing/invalid `structured_output`, or `stop_reason` indicating turn-limit exhaustion as
stage failure → request `failed` with a human-readable `error`.

### What the model does in the sandbox (contract, enforced by prompt + validation)

- Reads `mods.lock.json`, config files; searches registries via
  `jarjard modtool search|project|versions|deps` (JSON on stdout — see §8).
- Edits/creates config files **in place** under `config/`, `defaultconfigs/`,
  `kubejs/`, `scripts/` only.
- Emits the plan as its final structured answer; every file it changed appears as a
  `config_edit` action; mods to add/remove/update appear as actions with registry IDs
  taken from `modtool` output.

### Clarification loop

If `plan.status == "needs_clarification"`: store the question, set request
`awaiting_clarification`, stop (worktree removed). When the player answers
(`POST /requests/{id}/answer`), the daemon re-enqueues the job; the new run gets
`{{clarification_qa}}` filled. Maximum 2 clarification rounds; a third
`needs_clarification` fails the request with a friendly error.

If `plan.status == "infeasible"`: request → `infeasible`, summary shown to the player,
done.

## 3. Validate

All checks run against the worktree + plan. First failure aborts the job (`failed`,
with the check name and detail in `error`). Checks, in order:

| # | Check |
|---|---|
| V1 | Plan re-validates against `schemas/plan.schema.json` (worker-side validator). |
| V2 | `git diff --name-only` (worktree vs HEAD): every changed path is under `config/`, `defaultconfigs/`, `kubejs/`, or `scripts/`; is UTF-8 text; is < 1 MiB. Changes to `jarjar.pack.json`, `mods.lock.json`, `CLAUDE.md`, `.jarjar/**`, or anything else → **reject**. |
| V3 | Bijection: set of changed paths == set of `config_edit.path` values. |
| V4 | Every changed file parses according to extension: `.toml` (TOML 1.0), `.json` (strict), `.json5` (JSON5), `.yaml`/`.yml` (YAML 1.2), `.properties`/`.cfg` (java-properties lenient), `.snbt` (bracket/quote balance check only). Unknown extension → reject. |
| V5 | Each `add_mod`/`update_mod`: fetch the version from the registry by `version_id`; confirm it belongs to `project_id`, supports `mc_version`, and supports the pack loader (Modrinth: `game_versions` + `loaders`; CurseForge: `gameVersions` contains both the MC version and the loader name). `add_mod` for a project already in `mods.lock.json` → reject (should be `update_mod`). |
| V6 | Each `remove_mod`/`update_mod.from_path` exists in `mods.lock.json`. |
| V7 | Dependency closure: for each added/updated version, fetch required dependencies (transitively, depth ≤ 10). Any required project not already in the pack and not in the plan is **auto-added** by resolving its latest version compatible with `mc_version`+loader; auto-adds are appended to the changelog. Unresolvable required dependency → reject. |
| V8 | `server_property.key` must already exist in the live `server.properties` and must not be in the blocklist `{"level-name"}`. |
| V9 | If `plan.confidence == "low"` **or** config `pipeline.require_approval` is true: set `awaiting_approval` and stop here; resume from §4 on admin approval (the validated plan is stored in `requests.plan_json`). Rejection → status `rejected`, worktree removed. |

## 4. Materialize (deterministic, no AI)

For each `add_mod`/`update_mod` (including auto-added dependencies):

1. Download from the registry-provided URL to a temp file.
2. Verify the registry hash (Modrinth: `sha512`; CurseForge: `sha1`). Mismatch → fail.
3. Compute SHA-256, move into the blob store (`blobs/sha256/<aa>/<hash>`), no-op if
   already present.
4. Update `mods.lock.json`: add/replace/remove entries per the plan actions
   (`remove_mod` deletes its entry). Keep the file sorted by `path`.

## 5. Build

1. Walk the worktree (committed dirs only) and `mods.lock.json`; assemble the manifest
   `files` array per `DATA-CONTRACTS.md §4` (side classification precedence included).
   Store every config/script file as a blob too.
2. `git add -A && git commit` in the worktree with message
   `req <id>: <plan.summary first line>`; merge (fast-forward) into the pack repo's main
   branch; tag `v<N+1>`.
3. Write `manifests/<N+1>.json` (not yet announced).
4. Changelog: plan summary + one entry per action + auto-added dependencies.

## 6. Server apply

Only files with `side ∈ {server, both}` plus `server_property` actions.

1. Compute server delta: target file set vs the previous manifest's server file set.
   The daemon only ever touches **managed paths** (the allowed dirs + `mods/`) inside
   `minecraft.server_dir` — never `world/`, logs, or unknown files.
2. Stage: copy new/changed blobs into `<server_dir>/.jarjar-staging/`.
3. Wait for the restart window per `minecraft.restart_policy`:
   - `immediate`: RCON `say` warning at T-60 s and T-10 s.
   - `when_empty`: poll RCON `list` every 60 s; proceed when 0 players; after
     `when_empty_timeout_minutes`, fall back to `immediate` behavior. While waiting the
     request stays in `applying_server`.
4. Stop the server (`systemctl stop` or `stop_command`; SIGTERM path must allow ≥ 90 s
   for world save).
5. Back up every file about to be replaced/deleted to
   `/var/lib/jarjar/rollback-tmp/<version>/`. Apply the delta (write-temp-then-rename),
   apply `server_property` changes to `server.properties` (backed up too).
6. Start the server. **Health check**: systemd unit active *and* RCON connects
   within `boot_healthy_timeout_seconds`.
7. On health-check failure: restore the backup, start the server again, `git reset` the
   pack repo to tag `v<N>` and delete tag `v<N+1>`, delete `manifests/<N+1>.json`,
   request → `failed`. (If the restore boot also fails, emit `server_status: down` and
   leave intervention to the operator — never loop.)

## 7. Publish

1. Insert the `versions` row (this is the atomic "it exists" point).
2. Request → `published`, `requests.version = N+1`.
3. Emit `version_published` + final `request_updated` events. Tray apps light up.
4. Delete `rollback-tmp/<version>` and the worktree. Blob GC: delete blobs not
   referenced by the newest 10 manifests (config in `serve`, runs daily).

## 8. `jarjard modtool` (the model's registry window)

Subcommands (all print a single JSON document on stdout; exit 0 on success, 1 with
`{"error": "…"}` on failure). Global flags `--mc-version` and `--loader` default from
`jarjar.pack.json` found in the CWD.

| Command | Output |
|---|---|
| `jarjard modtool search <query> [--limit 8]` | `{ "results": [ { "platform", "project_id", "slug", "title", "description", "downloads", "client_side", "server_side" } ] }` — Modrinth first; CurseForge appended when `pipeline.allow_curseforge`. Results are pre-filtered to the pack's MC version + loader. |
| `jarjard modtool project <platform> <project_id>` | Full project metadata: title, description, categories, side info, links. |
| `jarjard modtool versions <platform> <project_id> [--limit 5]` | `{ "versions": [ { "version_id", "version_number", "mc_versions", "loaders", "date", "file_name", "dependencies": [ { "project_id", "kind": "required\|optional\|incompatible" } ] } ] }` — newest first, filtered to compatible. |
| `jarjard modtool installed` | The current `mods.lock.json` rendered with project titles. |

`modtool` performs real HTTP calls to the registries (10 s timeout, 3 retries,
`User-Agent: jarjar/<version> (github.com/akimbohh/JarJar)`); it is the same code the
validator uses (`internal/worker/modtool`), which is what makes "the model can only cite
what the registry actually said" enforceable.

## 9. Rollback & import jobs

- **Rollback** (`jarjard worker --rollback <n>` via `POST /admin/rollback`): loads
  `manifests/<n>.json`, restores that file set into a worktree (configs from blobs,
  `mods.lock.json` regenerated), then continues from stage 5 (Build) with
  `request_id = null`-style synthetic request, summary `"Rolled back to version <n>"`.
- **Import** (`jarjard import <file.mrpack | manifest.json + overrides dir>`, CLI, run
  once at setup): parses the Modrinth `.mrpack` (files + env + overrides) or CurseForge
  manifest (resolving each `projectID/fileID` via the CF API), downloads everything into
  the blob store, writes `jarjar.pack.json` + `mods.lock.json` + overrides into a fresh
  pack repo, commits as version 1, publishes manifest 1, and applies it to the server
  dir (stage 6 without the delta optimization). From then on, everything is diffs.
