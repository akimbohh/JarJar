# JarJar — Daemon HTTP API

Base path: `/api/v1`. All bodies are JSON (`Content-Type: application/json`) except blob
downloads. All endpoints require `Authorization: Bearer <token>` **except**
`POST /api/v1/join` and `GET /api/v1/health`.

## Errors

Non-2xx responses always have this body:

```json
{ "error": { "code": "not_found", "message": "no such request" } }
```

`code` values: `unauthorized` (401), `forbidden` (403), `not_found` (404),
`invalid_request` (400), `conflict` (409), `rate_limited` (429), `internal` (500).
Clients branch on `code` and HTTP status, never on `message`.

Rate limit: 60 requests/minute per token (blob downloads exempt). Exceeding returns 429
with a `Retry-After` header (seconds).

---

## Onboarding

### `POST /join` (no auth)

Redeem a single-use invite code for a player token.

Request: `{ "invite_code": "AB2C3D4E", "player_name": "alice" }`
Response `200`: `{ "token": "<base64url 43 chars>", "player_id": "plr_…", "role": "player", "server_name": "Our Pack" }`
Errors: `invalid_request` (bad/used code, name taken — message says which).

`player_name`: 1–32 chars, `[A-Za-z0-9_-]`, unique. The token is shown once; the daemon
stores only its SHA-256.

---

## Requests

### `POST /requests`

Submit a plain-English change request.

Request: `{ "text": "add a cave loot mod" }` (`text`: 1–2000 chars)
Response `201`: the request object (below). A `request_updated` event is also emitted.
Errors: `conflict` if another request is currently in a non-terminal state
(**one in-flight request at a time, globally** — the message names the blocking request).

**Request object** (returned by all request endpoints):

```json
{
  "id": "req_…", "player_id": "plr_…", "player_name": "alice",
  "text": "add a cave loot mod",
  "status": "planning",
  "summary": null,
  "question": null,
  "error": null,
  "version": null,
  "created_at": "…", "updated_at": "…"
}
```

`status` enum and transitions: see `DATA-CONTRACTS.md §7`. `summary` is populated from
the plan once planning succeeds. `question` is set while `awaiting_clarification`.
`version` is set once `published`.

### `GET /requests?limit=20&before=<req_id>`

List requests, newest first. Response `200`: `{ "requests": [ …request objects… ] }`.
`limit` default 20, max 100. `before` paginates by ID.

### `GET /requests/{id}`

Response `200`: request object. `404` if unknown.

### `POST /requests/{id}/answer`

Answer a clarification question. Only the requesting player (or an admin) may answer;
request must be `awaiting_clarification`.

Request: `{ "text": "the second one, Sophisticated Backpacks" }`
Response `200`: request object (status back to `planning`).
Errors: `conflict` (wrong status), `forbidden` (different player).

---

## Pack & sync

### `GET /pack/current`

Response `200`:

```json
{ "version": 42, "summary": "Added Sodium for better FPS.",
  "pack": { "name": "Our Pack", "mc_version": "1.21.1",
            "loader": { "id": "neoforge", "version": "21.1.77" } } }
```

`version` is `0` with `summary: ""` before the first import.

### `GET /pack/manifests/{n}`

Response `200`: the immutable version-`n` manifest (`schemas/manifest.schema.json`),
`Cache-Control: immutable, max-age=31536000`. `404` for unknown or pruned versions.

### `GET /blobs/{sha256}`

Response `200`: raw bytes, `Content-Type: application/octet-stream`,
`Content-Length` set, supports `Range` requests (`206`) and `HEAD`. `404` if the blob
does not belong to any retained manifest. The `{sha256}` path segment must be 64
lowercase hex chars (`400` otherwise).

### `GET /versions?limit=20&before=<n>`

Changelog feed, newest first. Response `200`:
`{ "versions": [ { "number": 42, "created_at": "…", "summary": "…", "request_id": "req_…"|null,
"changelog": [ {"kind": "added_mod", "text": "…"} ] } ] }`

---

## Events (long-poll)

### `GET /events?cursor=<seq>&timeout=55`

Returns events with `seq > cursor` (schema: `schemas/events.schema.json`). If none exist,
the request blocks until one arrives or `timeout` seconds elapse (max 55; default 55),
then returns an empty list.

Response `200`: `{ "events": [ … ], "cursor": 1041 }` — `cursor` echoes the highest
`seq` returned (or the input cursor when empty). Clients persist the cursor and pass it
back. `cursor=0` returns only events from now on **plus** the daemon fast-forwards the
client by immediately returning `cursor` = latest seq with an empty list (clients then
reconcile state via `GET /pack/current`).

---

## Health

### `GET /health` (no auth)

Response `200`: `{ "status": "ok", "version": "<jarjard semver>", "pack_version": 42,
"mc_server": "running" }`. Used by clients to test the URL during setup.

---

## Admin (role = `admin`)

All under `/admin`; `forbidden` for non-admin tokens.

| Method & path | Body → Response |
|---|---|
| `POST /admin/invites` | `{ "role": "player" }` → `201 { "invite_code": "AB2C3D4E" }` |
| `GET /admin/players` | → `{ "players": [ { "id", "name", "role", "created_at" } ] }` |
| `DELETE /admin/players/{id}` | → `204` (revokes token; cannot delete last admin) |
| `POST /admin/requests/{id}/approve` | → `200` request object (must be `awaiting_approval`) |
| `POST /admin/requests/{id}/reject` | `{ "reason": "…" }` → `200` request object (status `rejected`) |
| `POST /admin/rollback` | `{ "to_version": 41 }` → `202 { "request_id": "req_…" }` — enqueues a rollback job that publishes a new version whose content equals version 41 and applies it to the server |
| `GET /admin/status` | → `{ "queue": [...request ids...], "current_job": "req_…"\|null, "mc_server": "running", "disk_free_bytes": … }` |

The CLI verbs `jarjard invite`, `jarjard rollback`, `jarjard status` call these endpoints
over the loopback listener using a locally-stored admin token
(`/var/lib/jarjar/secrets/admin-token`, created on first `jarjard serve` start).

---

## Versioning & compatibility

- Breaking wire changes bump `/api/v1` → `/api/v2`; additive fields do not.
- Clients send `User-Agent: jarjar-client/<semver> (<os>)`. The daemon includes
  `X-JarJar-Min-Client: <semver>` on every response; a client older than that shows an
  "update JarJar" notice and disables Apply.
