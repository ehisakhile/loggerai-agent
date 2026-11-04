#!/bin/sh
# Installs the LoggerAI server agent on Linux, as a service that runs as its own unprivileged user.
#
#   curl -fsSL https://github.com/ehisakhile/loggerai-agent/releases/latest/download/install.sh | sudo LOGGERAI_KEY=sk_live_... sh
#
# Run it again to upgrade (your settings stay). "sh install.sh uninstall" removes it.
set -eu

REPO="https://github.com/ehisakhile/loggerai-agent/releases"
VERSION="${LOGGERAI_VERSION:-latest}"
BIN=/usr/local/bin/loggerai-agent
CONF=/etc/loggerai/agent.conf
UNIT=/etc/systemd/system/loggerai-agent.service
USER_NAME=loggerai

say() { printf '%s\n' "$*"; }
die() { printf 'LoggerAI agent: %s\n' "$*" >&2; exit 1; }

[ "$(uname -s)" = Linux ] || die "this agent runs on Linux."
[ "$(id -u)" = 0 ] || die "run this as root, with sudo."
command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ] || die "this installer needs systemd. Without it, run the binary from the releases page yourself."

if [ "${1:-}" = uninstall ]; then
  systemctl disable --now loggerai-agent 2>/dev/null || true
  rm -f "$UNIT" "$BIN" "$CONF"
  rmdir /etc/loggerai 2>/dev/null || true
  systemctl daemon-reload
  userdel "$USER_NAME" 2>/dev/null || true
  say "LoggerAI agent removed."
  exit 0
fi

case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) die "no build for $(uname -m) yet." ;;
esac

KEY="${LOGGERAI_KEY:-}"
if [ -z "$KEY" ] && [ ! -f "$CONF" ]; then
  [ -r /dev/tty ] || die "set LOGGERAI_KEY to your project's secret key."
  printf 'Project secret key (sk_live_...): ' >/dev/tty
  stty -echo </dev/tty 2>/dev/null || true
  read -r KEY </dev/tty
  stty echo </dev/tty 2>/dev/null || true
  printf '\n' >/dev/tty
fi
case "$KEY" in
  "" | sk_*) ;;
  *) die "that isn't a secret key. Find it under Connect in LoggerAI: it starts sk_live_." ;;
esac

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
if [ -n "${LOGGERAI_BINARY:-}" ]; then
  cp "$LOGGERAI_BINARY" "$TMP/agent" # for testing a build that isn't released yet
else
  case "$VERSION" in
    latest) BASE="$REPO/latest/download" ;;
    *) BASE="$REPO/download/$VERSION" ;;
  esac
  FILE="loggerai-agent-linux-$ARCH"
  say "Downloading $FILE..."
  curl -fsSL "$BASE/$FILE" -o "$TMP/$FILE" || die "the download failed."
  curl -fsSL "$BASE/checksums.txt" -o "$TMP/checksums.txt" || die "the checksums wouldn't download."
  (cd "$TMP" && grep " $FILE\$" checksums.txt | sha256sum -c --status) || die "the download doesn't match its checksum, so nothing was installed."
  mv "$TMP/$FILE" "$TMP/agent"
fi

id "$USER_NAME" >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin "$USER_NAME"
install -m 0755 "$TMP/agent" "$BIN"
install -d -m 0750 -o root -g "$USER_NAME" /etc/loggerai

if [ -n "$KEY" ]; then
  # Written by the shell, so the key never appears in a process list.
  OLD_UMASK="$(umask)"
  umask 027
  {
    say "# LoggerAI agent settings. This file holds your secret key."
    say "key=$KEY"
    [ -z "${LOGGERAI_ENDPOINT:-}" ] || say "endpoint=$LOGGERAI_ENDPOINT"
    [ -z "${LOGGERAI_ENVIRONMENT:-}" ] || say "environment=$LOGGERAI_ENVIRONMENT"
    [ -z "${LOGGERAI_HOST:-}" ] || say "host=$LOGGERAI_HOST"
    [ -z "${LOGGERAI_ALLOW_HTTP:-}" ] || say "allow_http=$LOGGERAI_ALLOW_HTTP"
  } >"$CONF"
  umask "$OLD_UMASK"
  chown root:"$USER_NAME" "$CONF"
  chmod 0640 "$CONF"
fi

cat >"$UNIT" <<UNIT
[Unit]
Description=LoggerAI server agent
After=network.target

[Service]
ExecStart=$BIN run
User=$USER_NAME
Restart=always
RestartSec=10
NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
MemoryMax=128M
CPUQuota=10%

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable loggerai-agent >/dev/null 2>&1
systemctl restart loggerai-agent

if su -s /bin/sh "$USER_NAME" -c "$BIN check"; then
  say "The LoggerAI agent is running. Logs: journalctl -u loggerai-agent"
else
  say "The agent is installed but couldn't report. See the message above, then: journalctl -u loggerai-agent"
  exit 1
fi
