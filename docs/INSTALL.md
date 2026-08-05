# JarJar — Install & Operate

This is the server-side install for the `jarjard` daemon plus the one-time pack
import. Players install the tray client separately (see `client/README.md`).

## Prerequisites

- Linux server (x86-64 or arm64) with ~10 GB RAM, already running a modded
  Minecraft server as a **systemd unit** (or via start/stop commands).
- RCON enabled on the Minecraft server (`enable-rcon=true`, `rcon.password=…`,
  `rcon.port=25575` in `server.properties`).
- **Claude Code** CLI installed and on `PATH`, authenticated with a Claude
  Pro/Max subscription (see step 3).
- Go ≥ 1.24 to build, or a prebuilt `jarjard` binary.
- `git` on `PATH` (the pack store is a git repo).

## 1. Build & install the daemon

```sh
cd daemon
go build -ldflags "-X main.version=$(git describe --tags --always)" -o jarjard ./cmd/jarjard
sudo install -m 0755 jarjard /usr/local/bin/jarjard
```

Create the service user and data dir:

```sh
sudo useradd --system --home /var/lib/jarjar --shell /usr/sbin/nologin jarjar || true
sudo mkdir -p /var/lib/jarjar /etc/jarjar
sudo chown -R jarjar:jarjar /var/lib/jarjar
```

## 2. Configure

```sh
sudo cp daemon/jarjard.example.toml /etc/jarjar/jarjard.toml
sudo $EDITOR /etc/jarjar/jarjard.toml    # set server_dir, rcon_password, etc.
```

Give the `jarjar` user permission to control the Minecraft unit and read its
directory. If the game server runs as another user, grant a polkit rule or run
both as the same user.

## 3. Authenticate Claude Code (subscription token)

The pipeline runs Claude Code headless with a **subscription** token, never an
API key:

```sh
sudo -u jarjar claude setup-token          # opens a browser flow; prints a token
sudo -u jarjar mkdir -p /var/lib/jarjar/secrets
# paste the printed token:
sudo -u jarjar tee /var/lib/jarjar/secrets/claude-token >/dev/null
sudo chmod 600 /var/lib/jarjar/secrets/claude-token
```

The token is long-lived (~1 year). When it expires the daemon logs an auth
failure — re-run `claude setup-token`.

> **Memory tip:** add ~2 GB of zram swap so transient worker + Claude Code peaks
> page against the mostly-idle JVM heap instead of OOMing. The worker and Claude
> run in a `systemd-run` scope capped at `claude.memory_max`.

## 4. Import your existing pack (version 1)

Export your current pack as a Modrinth `.mrpack` (or a CurseForge zip), then:

```sh
sudo -u jarjar jarjard import /path/to/pack.mrpack --config /etc/jarjar/jarjard.toml
# CurseForge packs additionally need pipeline.allow_curseforge = true + an API key.
```

This downloads every mod into the blob store, writes the pack repo, and
publishes **version 1**. From here on, everything is diffs driven by requests.

## 5. Run the daemon

```sh
sudo cp deploy/jarjard.service /etc/systemd/system/jarjard.service
sudo systemctl daemon-reload
sudo systemctl enable --now jarjard
sudo systemctl status jarjard
```

On first start the daemon mints an admin token at
`/var/lib/jarjar/secrets/admin-token` (used by the CLI verbs).

Check it's up:

```sh
curl -s http://127.0.0.1:25580/api/v1/health | jq
```

## 6. Invite players

```sh
sudo -u jarjar jarjard invite --config /etc/jarjar/jarjard.toml
# → prints an 8-char invite code; players enter it + the server URL in the tray app.
```

Mint an admin invite with `--role admin`.

## 7. Day-to-day

```sh
sudo -u jarjar jarjard status                     # queue + server state
sudo -u jarjar jarjard rollback --to 5            # roll the pack back to version 5
sudo -u jarjar jarjard invite --role player       # more invites
```

Players type requests in the tray app; the daemon plans, validates, applies, and
restarts the server per your `restart_policy`. A failed version auto-rolls-back
and the request is marked failed with the reason.

## Networking / TLS

The daemon speaks plain HTTP by default. For production choose one:

- **VPN** (WireGuard/Tailscale) — simplest; keep `listen_addr` on the VPN iface.
- **Reverse proxy** (Caddy/nginx) terminating TLS in front of `listen_addr`.
- **Built-in TLS** — set `tls_cert_file` / `tls_key_file` in the config.

## Security notes

- Player tokens are stored hashed (SHA-256); the plaintext is shown once.
- The Claude Code run is sandboxed: tools limited to file edits +
  `Bash(jarjard modtool *)`, network tools disabled, working directory scoped to
  a throwaway git worktree, secrets denied. The git-diff allowlist is the real
  boundary — even a misbehaving model can only change config paths, and the
  validator re-verifies every mod against the registries.
