# JarJar — Server setup

The Linux box runs everything: Claude, the pack builder/provisioner, and the
Minecraft server. Windows (or macOS/Linux) runs only the **desktop app**. There
are two ways to set up a server; the first needs no command-typing beyond a
single install line.

## A. App-driven setup (recommended)

On the Linux box, run the one-command installer:

```sh
curl -fsSL https://raw.githubusercontent.com/akimbohh/JarJar/main/deploy/bootstrap.sh | sudo bash
```

It installs the daemon + dependencies (Java, Claude Code), starts `jarjard` in an
**unconfigured** state, and prints a **server URL** and an **admin code**. That is
the only thing you type on the server.

Then, in the **JarJar desktop app**:

1. Choose "Set up a server" and enter the URL + admin code.
2. Describe the modpack you want and paste a Claude Code token
   (get one on any machine with Claude Code: `claude setup-token`).
3. Watch it build. The app calls the daemon's setup API, which runs the same
   genesis → provision → build → boot pipeline as `jarjard init` below, streaming
   progress back to the app. When it finishes, the server is up and the app shows
   the dashboard.

Under the hood the app uses these daemon endpoints (admin-authed):
`GET /api/v1/admin/setup` (configured/running + live `steps[]`) and
`POST /api/v1/admin/setup` (`{description, claude_token, memory_mb?, server_dir?,
server_port?, allow_curseforge?, curseforge_key?}`). Progress also streams on the
`GET /api/v1/events` feed as `setup_progress` events.

## B. One-command CLI setup (`jarjard init`)

`jarjard init` does the same thing from the shell — a single plain-English
description turned into a running, **optimized, always-on** server plus pack
version 1. Use it when you'd rather not use the app for setup, or to script it.

This is one-time setup. The Minecraft server is meant to stay up: it is
configured to auto-restart and boot on machine start. It is **not** a per-change
cold boot — ongoing edits never re-provision the server.

## What it does, in order

1. **Genesis (AI planning).** Runs Claude Code headless in a throwaway dir whose
   only tool is the `jarjard modtool` registry CLI. From the description it
   decides the Minecraft version and mod loader, then either:
   - finds an **existing published pack** on Modrinth/CurseForge (`modtool
     packsearch` → `packversions`) and cites its `version_id`, or
   - assembles one **from scratch** — searching mods, resolving compatible
     `version_id`s, and listing them.

   Output is a structured `GenesisPlan` (see `schemas/genesis.schema.json`).
   Every id is one the registry actually returned — the model cannot cite what
   it did not look up.

2. **Provision the server** (`internal/provision`). Installs the loader server
   (Fabric/NeoForge/Forge/Quilt) for the chosen MC version, accepts the EULA,
   writes a `server.properties` with RCON enabled and **performance defaults**
   (view/simulation distance, network compression, watchdog disabled for modded
   worldgen — all fill-if-absent so later tuning survives), generates a random
   RCON password, and emits:
   - a start script running the server with **Aikar's G1GC flags** sized to the
     configured heap, and
   - an **always-restart** systemd unit (`Restart=always`, boots on
     `multi-user.target`).

   Provision never writes to `/etc` or starts the process itself — that is the
   `init` step below, which needs root.

3. **Build pack version 1.** For an existing pack, the resolved `.mrpack` is
   downloaded (hash-verified) and run through the standard importer. For a
   scratch pack, a fresh pack repo is initialized, each mod jar is downloaded
   and hash-verified into the content-addressed blob store, the lock is written,
   and version 1 is committed and published.

4. **Seed the server dir.** Version 1's server-side files (`side` server/both,
   under the managed content dirs) are copied from the blob store into the live
   server directory so the freshly provisioned server boots with the pack in
   place.

5. **Install + boot** (with `--install-systemd`, default on). Writes the unit
   under `/etc/systemd/system`, `daemon-reload`, `enable`, then `start` and waits
   for a healthy boot (RCON reachable). With `--boot=false` it installs but does
   not start; with `--install-systemd=false` it prints the unit for manual
   install.

6. **Mint the first invite.** Prints an admin invite code for the owner's own
   tray client.

## Usage

```sh
sudo jarjard init \
  --description "a cozy create-mod pack for 1.20.1 with farming and building mods" \
  --token "$CLAUDE_CODE_OAUTH_TOKEN" \
  --server-dir /opt/minecraft \
  --data-dir /var/lib/jarjar \
  --public-url https://mc.example.com:25580 \
  --memory-mb 9216
```

Key flags (see `jarjard init -h` for all):

| flag | default | meaning |
|------|---------|---------|
| `--description` | — | the pack you want, in plain English (also accepted as a trailing arg) |
| `--token` / `--token-file` | — | Claude Code OAuth token (Pro/Max) |
| `--server-dir` | `/opt/minecraft` | where the Minecraft server is installed |
| `--data-dir` | `/var/lib/jarjar` | daemon data (pack repo, blobs, DB) |
| `--memory-mb` | `9216` | JVM heap; Aikar region sizing shifts at ≥12 GB |
| `--server-port` | `25565` | Minecraft port |
| `--allow-curseforge` / `--curseforge-key` | off | enable CurseForge as a source |
| `--service-user` | — | systemd `User=` for the server |
| `--install-systemd` | `true` | write + enable the unit (needs root) |
| `--boot` | `true` | start and health-check after install |

Prerequisites: Java (for the loader installer + server), `git`, and Claude Code
on `PATH`. On success `init` writes `/etc/jarjar/jarjard.toml` (with the
generated RCON password) — start the daemon with `jarjard serve`.

## Relationship to the manual install

`docs/INSTALL.md` documents the manual path for an operator who already runs a
Minecraft server and wants to point JarJar at it (`jarjard import` a `.mrpack`).
`jarjard init` is the turnkey alternative that also stands up and tunes the
server. Both converge on the same state: a published pack version 1, a
configured daemon, and an invite code.
