#!/usr/bin/env bash
#
# JarJar one-command server install.
#
#   curl -fsSL https://raw.githubusercontent.com/akimbohh/JarJar/main/deploy/bootstrap.sh | sudo bash
#
# Installs the jarjard daemon + its dependencies (Java, Claude Code), starts it
# in an *unconfigured* state, and prints a server URL + admin code. Everything
# after that — describing the modpack, letting the AI build it, provisioning and
# booting the Minecraft server — is driven from the JarJar desktop app. No more
# commands on this box.
#
# Debian/Ubuntu (apt) x86-64 or arm64. Re-runnable.
#
# Overrides via env: JARJAR_VERSION, DATA_DIR, SERVER_DIR, LISTEN_ADDR, PUBLIC_URL.

set -euo pipefail

REPO="akimbohh/JarJar"
DATA_DIR="${DATA_DIR:-/var/lib/jarjar}"
SERVER_DIR="${SERVER_DIR:-/opt/minecraft}"
LISTEN_ADDR="${LISTEN_ADDR:-0.0.0.0:25580}"
CONFIG_PATH="/etc/jarjar/jarjard.toml"
SERVICE_PATH="/etc/systemd/system/jarjard.service"

say()  { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m warn:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = "0" ] || die "run as root (pipe to 'sudo bash')."
command -v apt-get >/dev/null 2>&1 || die "this installer supports Debian/Ubuntu (apt) only."

# ---- architecture -----------------------------------------------------------
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

# ---- resolve the release version --------------------------------------------
if [ -z "${JARJAR_VERSION:-}" ]; then
  JARJAR_VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep -oE '"tag_name":[[:space:]]*"[^"]+"' | head -1 | grep -oE 'v[0-9][^"]*' || true)"
fi
[ -n "${JARJAR_VERSION:-}" ] || die "could not determine the latest JarJar version; set JARJAR_VERSION=vX.Y.Z"
say "Installing JarJar ${JARJAR_VERSION} (${ARCH})"

# ---- dependencies -----------------------------------------------------------
say "Installing dependencies (Java, git, Node, Claude Code)…"
export DEBIAN_FRONTEND=noninteractive
apt-get update -y
apt-get install -y ca-certificates curl git openjdk-21-jre-headless

if ! command -v node >/dev/null 2>&1; then
  curl -fsSL https://deb.nodesource.com/setup_22.x | bash -
  apt-get install -y nodejs
fi
if ! command -v claude >/dev/null 2>&1; then
  npm install -g @anthropic-ai/claude-code
fi

# ---- daemon binary ----------------------------------------------------------
say "Downloading jarjard…"
ASSET="jarjard-linux-${ARCH}"
curl -fsSL -o /usr/local/bin/jarjard \
  "https://github.com/${REPO}/releases/download/${JARJAR_VERSION}/${ASSET}"
chmod 0755 /usr/local/bin/jarjard

# ---- dirs + unconfigured config ---------------------------------------------
say "Preparing ${DATA_DIR} and ${CONFIG_PATH}…"
mkdir -p "$DATA_DIR/secrets" "$SERVER_DIR" /etc/jarjar
chmod 0700 "$DATA_DIR/secrets"

PUBLIC_LINE=""
if [ -n "${PUBLIC_URL:-}" ]; then
  PUBLIC_LINE="public_url = \"${PUBLIC_URL}\""
fi

if [ ! -f "$CONFIG_PATH" ]; then
  cat >"$CONFIG_PATH" <<TOML
# JarJar daemon config. Written unconfigured by bootstrap.sh; the desktop app
# fills in the Minecraft + Claude details during setup.
[server]
listen_addr = "${LISTEN_ADDR}"
data_dir = "${DATA_DIR}"
${PUBLIC_LINE}

[minecraft]
server_dir = "${SERVER_DIR}"
control = "systemd"
systemd_unit = "minecraft.service"

[claude]
token_file = "${DATA_DIR}/secrets/claude-token"

[pipeline]
require_approval = false
TOML
  chmod 0600 "$CONFIG_PATH"
else
  say "Keeping existing ${CONFIG_PATH}."
fi

# ---- systemd service (runs as root so setup can install the MC unit) --------
say "Installing the jarjard service…"
cat >"$SERVICE_PATH" <<UNIT
[Unit]
Description=JarJar daemon (AI modpack manager)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/jarjard serve --config ${CONFIG_PATH}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now jarjard

# ---- wait for health --------------------------------------------------------
say "Waiting for the daemon to come up…"
HEALTH="http://127.0.0.1:${LISTEN_ADDR##*:}/api/v1/health"
for _ in $(seq 1 30); do
  if curl -fsS "$HEALTH" >/dev/null 2>&1; then break; fi
  sleep 1
done
curl -fsS "$HEALTH" >/dev/null 2>&1 || warn "daemon health check didn't pass yet; check: journalctl -u jarjard -e"

# ---- mint an admin invite for the app ---------------------------------------
say "Creating an admin code for the app…"
INVITE_OUT="$(jarjard invite --role admin --config "$CONFIG_PATH" 2>/dev/null || true)"
CODE="$(printf '%s\n' "$INVITE_OUT" | grep -oE '[A-Z0-9]{8}' | head -1 || true)"

# ---- reachable address ------------------------------------------------------
IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
URL="${PUBLIC_URL:-http://${IP:-YOUR_SERVER_IP}:${LISTEN_ADDR##*:}}"

cat <<DONE

============================================================
  JarJar is installed and running. Finish setup in the app.
============================================================

  Server URL:  ${URL}
  Admin code:  ${CODE:-<run: jarjard invite --role admin>}

  1. Install the JarJar desktop app (Windows/macOS/Linux):
       https://github.com/${REPO}/releases/latest
  2. Open it, choose "Set up a server", and enter the URL + admin code above.
  3. Describe the modpack you want and paste a Claude Code token
     (get one on any machine with Claude Code:  claude setup-token).
  The app drives the rest — the AI builds the pack and boots the server.

  Logs:   journalctl -u jarjard -f
============================================================
DONE
