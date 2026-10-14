#!/bin/sh
# needle-client one-shot installer.
#
# Fetches the agent binary, drops it at a fixed path, picks whichever
# supervision mechanism the host actually has, and starts it. Designed for the
# whole fleet: Debian boxes with systemd, OpenWrt routers with procd, and hosts
# with neither. Needs only curl or wget; no repository, no Go, no root.
#
#   curl -fsSL https://raw.githubusercontent.com/justypist/needle/main/deploy/client/install.sh \
#     | sh -s -- --url https://probe.example.com --token <token>
#
#   ./install.sh --url ... --token ... --method tmux
#   ./install.sh --uninstall
#
# The binary comes from a GitHub release, by default the rolling `edge` build
# of main. Pin a version with --version v1.2.3, or point --base-url elsewhere.
# If a needle-client binary sits next to this script it is used instead, which
# is what the offline path does.
#
# Method is detected in this order unless --method says otherwise:
#
#   systemd --user  a per-user unit, no root and no sudo needed
#   procd           OpenWrt/iStoreOS: an init script under /etc/init.d
#   tmux            a detached session that survives logout
#   nohup           last resort: a plain background process
set -eu

BIN_NAME="needle-client"
DEFAULT_BIN="/usr/local/bin/$BIN_NAME"
DEFAULT_USER_BIN="$HOME/.local/bin/$BIN_NAME"
UNIT_NAME="needle-client"
PROCD_NAME="needle-client"
SESSION="needle-client"

REPO="${NEEDLE_REPO:-justypist/needle}"
VERSION="${NEEDLE_VERSION:-edge}"

# $0 is unreliable when this runs as `curl ... | sh`, so resolve the script
# directory only when it actually came from a file.
SRC_DIR=""
case "${0:-}" in
  ""|-|sh|dash|bash) ;;
  *) [ -f "$0" ] && SRC_DIR="$(cd "$(dirname "$0")" && pwd)" ;;
esac

URL="${NEEDLE_URL:-}"
TOKEN="${NEEDLE_TOKEN:-}"
INTERVAL="${NEEDLE_INTERVAL:-30s}"
METHOD="${NEEDLE_METHOD:-auto}"
BIN="${NEEDLE_BIN:-}"
BASE_URL="${NEEDLE_BASE_URL:-}"
UNINSTALL=0
VERBOSE=0

log()  { printf '\033[36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m[fail]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<EOF
usage: $0 --url <server> --token <token> [options]

  --url <url>        needle-server base url (required)
  --token <token>    agent token (required)
  --interval <dur>   report interval (default 30s)
  --method <m>       systemd-user | procd | tmux | nohup | auto (default auto)
  --bin <path>       install path for the binary
  --version <v>      release to download: edge, latest, or v1.2.3 (default edge)
  --base-url <url>   download root, e.g. a self-hosted mirror
  --uninstall        stop and remove the agent
  -v                 verbose
  -h, --help         this message

Environment equivalents: NEEDLE_URL, NEEDLE_TOKEN, NEEDLE_INTERVAL,
NEEDLE_METHOD, NEEDLE_BIN, NEEDLE_VERSION, NEEDLE_BASE_URL, NEEDLE_REPO.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --url) URL="${2:-}"; shift 2 ;;
    --token) TOKEN="${2:-}"; shift 2 ;;
    --interval) INTERVAL="${2:-}"; shift 2 ;;
    --method) METHOD="${2:-}"; shift 2 ;;
    --bin) BIN="${2:-}"; shift 2 ;;
    --version) VERSION="${2:-}"; shift 2 ;;
    --base-url) BASE_URL="${2:-}"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    -v|--verbose) VERBOSE=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1 (try --help)" ;;
  esac
done

# --------------------------------------------------------------------------
# capability probes
# --------------------------------------------------------------------------

have() { command -v "$1" >/dev/null 2>&1; }

has_systemd_user() {
  have systemctl || return 1
  # A container or a non-systemd init still ships systemctl sometimes; the
  # runtime directory is the honest test for a working user manager.
  [ -S "/run/user/$(id -u)/systemd/private" ] || [ -d "/run/user/$(id -u)/systemd" ] || return 1
  systemctl --user show-environment >/dev/null 2>&1
}

has_procd() {
  [ -d /etc/init.d ] || return 1
  have ubus || have procd || [ -f /sbin/procd ] || return 1
  # Writing an init script needs root.
  [ "$(id -u)" = 0 ]
}

can_write() { # dir
  [ -d "$1" ] && [ -w "$1" ]
}

pick_bin_path() {
  if [ -n "$BIN" ]; then
    echo "$BIN"
    return
  fi
  # Prefer a directory that is on PATH and writable without sudo.
  for d in /usr/local/bin /usr/bin "$HOME/.local/bin"; do
    if can_write "$d"; then
      echo "$d/$BIN_NAME"
      return
    fi
  done
  mkdir -p "$HOME/.local/bin"
  echo "$DEFAULT_USER_BIN"
}

detect_method() {
  if [ "$METHOD" != auto ]; then
    echo "$METHOD"
    return
  fi
  if has_procd; then echo procd; return; fi
  if has_systemd_user; then echo systemd-user; return; fi
  if have tmux; then echo tmux; return; fi
  echo nohup
}

# --------------------------------------------------------------------------
# binary
# --------------------------------------------------------------------------

arch_tag() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    armv7l|armv6l|arm) echo arm ;;
    mips*) echo mipsle ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
}

# local_binary looks for a build we can install without touching the network.
# Tried before any download so the offline path keeps working.
local_binary() {
  [ -n "$SRC_DIR" ] || return 1
  for cand in \
    "$SRC_DIR/$BIN_NAME" \
    "$SRC_DIR/${BIN_NAME}-linux-$(arch_tag)" \
    "$SRC_DIR/../dist/${BIN_NAME}-linux-$(arch_tag)" \
    "$SRC_DIR/../dist/$BIN_NAME"; do
    if [ -f "$cand" ]; then echo "$cand"; return 0; fi
  done
  return 1
}

# installed_binary is the last resort: an agent is already on this host, so
# re-running the installer can still rewrite the service definition.
installed_binary() {
  for cand in "$DEFAULT_USER_BIN" "$DEFAULT_BIN"; do
    if [ -x "$cand" ]; then echo "$cand"; return 0; fi
  done
  return 1
}

fetch() { # url dest
  if have curl; then
    curl -fsSL --retry 3 --connect-timeout 15 -o "$2" "$1"
  elif have wget; then
    wget -q -O "$2" "$1"
  else
    die "neither curl nor wget is available"
  fi
}

# download_binary writes the release asset to $1. The asset names come from
# scripts/build.sh: needle-client-linux-<arch>.
download_binary() {
  local dest="$1" url
  if [ -n "$BASE_URL" ]; then
    url="${BASE_URL%/}/$BIN_NAME-linux-$(arch_tag)"
  else
    url="https://github.com/$REPO/releases/download/$VERSION/$BIN_NAME-linux-$(arch_tag)"
  fi
  log "downloading $url"
  mkdir -p "$(dirname "$dest")"
  # Download beside the target and rename, so an interrupted transfer cannot
  # leave a truncated binary where a service would exec it.
  fetch "$url" "$dest.part" || die "download failed: $url"
  mv -f "$dest.part" "$dest"
}

# --------------------------------------------------------------------------
# supervision backends
# --------------------------------------------------------------------------

unit_dir() { echo "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"; }
unit_path() { echo "$(unit_dir)/$UNIT_NAME.service"; }
env_path() { echo "$(unit_dir)/$UNIT_NAME.env"; }

install_systemd_user() { # bin
  local bin="$1" dir u e
  dir="$(unit_dir)"
  u="$(unit_path)"
  e="$(env_path)"
  mkdir -p "$dir"

  cat > "$e" <<EOF
NEEDLE_URL=$URL
NEEDLE_TOKEN=$TOKEN
NEEDLE_INTERVAL=$INTERVAL
EOF
  chmod 600 "$e"

  cat > "$u" <<EOF
[Unit]
Description=needle metrics agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$bin
EnvironmentFile=$e
Restart=always
RestartSec=10

# The agent only reads /proc and /sys and speaks outbound HTTPS. Nothing it
# does needs privileges, so it is fenced in by construction.
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
SystemCallArchitectures=native

[Install]
WantedBy=default.target
EOF

  systemctl --user daemon-reload
  systemctl --user enable --now "$UNIT_NAME.service"

  # Without lingering, a user manager is torn down at logout, which would
  # stop the agent on a box nobody keeps logged in.
  if have loginctl; then
    if ! loginctl show-user "$(id -un)" 2>/dev/null | grep -q 'Linger=yes'; then
      loginctl enable-linger "$(id -un)" 2>/dev/null \
        || warn "could not enable lingering; run: sudo loginctl enable-linger $(id -un)"
    fi
  fi
  log "installed as a systemd user unit: $u"
}

install_procd() { # bin
  local bin="$1" script="/etc/init.d/$PROCD_NAME"
  cat > "$script" <<EOF
#!/bin/sh /etc/rc.common
# needle metrics agent (installed by needle install.sh)

START=95
STOP=10
USE_PROCD=1

PROG="$bin"

start_service() {
	procd_open_instance
	procd_set_param command "\$PROG"
	procd_set_param env "NEEDLE_URL=$URL" "NEEDLE_TOKEN=$TOKEN" "NEEDLE_INTERVAL=$INTERVAL"
	procd_set_param respawn 3600 5 5
	procd_set_param stdout 1
	procd_set_param stderr 1
	procd_close_instance
}

service_triggers() {
	procd_add_reload_trigger "$PROCD_NAME"
}
EOF
  # 700, not 755: the script carries the agent token.
  chmod 700 "$script"
  "$script" enable
  "$script" restart
  log "installed as a procd service: $script"
}

install_tmux() { # bin
  local bin="$1" cmd
  tmux kill-session -t "$SESSION" 2>/dev/null || true
  # Pass the credentials through the environment of the new session rather
  # than the command line, so they do not appear in `ps` output.
  cmd="export NEEDLE_URL='$URL' NEEDLE_TOKEN='$TOKEN' NEEDLE_INTERVAL='$INTERVAL'; exec '$bin'"
  tmux new-session -d -s "$SESSION" "$cmd"
  log "started in a detached tmux session: $SESSION"
  warn "tmux survives logout but not a reboot; re-run this script after a restart"
}

install_nohup() { # bin
  local bin="$1" pidfile="$HOME/.needle-client.pid" logf="$HOME/.needle-client.log"
  if [ -f "$pidfile" ] && kill -0 "$(cat "$pidfile")" 2>/dev/null; then
    kill "$(cat "$pidfile")" 2>/dev/null || true
    sleep 1
  fi
  # umask 077 keeps the log (which is normally quiet) private.
  ( umask 077; NEEDLE_URL="$URL" NEEDLE_TOKEN="$TOKEN" NEEDLE_INTERVAL="$INTERVAL" \
      nohup "$bin" >> "$logf" 2>&1 & echo $! > "$pidfile" )
  log "started with nohup: pid $(cat "$pidfile"), log $logf"
  warn "nohup does not survive a reboot; re-run this script after a restart"
}

# --------------------------------------------------------------------------
# uninstall
# --------------------------------------------------------------------------

# do_uninstall removes the service and the installed binary. `bin` is the
# install path, never the build artifact the script was run from.
do_uninstall() {
  local method="$1" bin="$2"
  case "$method" in
    systemd-user)
      systemctl --user disable --now "$UNIT_NAME.service" 2>/dev/null || true
      rm -f "$(unit_path)" "$(env_path)"
      systemctl --user daemon-reload 2>/dev/null || true
      ;;
    procd)
      if [ -f "/etc/init.d/$PROCD_NAME" ]; then
        "/etc/init.d/$PROCD_NAME" stop 2>/dev/null || true
        "/etc/init.d/$PROCD_NAME" disable 2>/dev/null || true
        rm -f "/etc/init.d/$PROCD_NAME"
      fi
      ;;
    tmux)
      tmux kill-session -t "$SESSION" 2>/dev/null || true
      ;;
    nohup)
      if [ -f "$HOME/.needle-client.pid" ]; then
        kill "$(cat "$HOME/.needle-client.pid")" 2>/dev/null || true
        rm -f "$HOME/.needle-client.pid"
      fi
      ;;
  esac
  if [ -f "$bin" ]; then
    rm -f "$bin"
    log "removed $bin"
  fi
  log "uninstalled ($method)"
  exit 0
}

# --------------------------------------------------------------------------
# main
# --------------------------------------------------------------------------

METHOD="$(detect_method)"

if [ "$UNINSTALL" = 1 ]; then
  do_uninstall "$METHOD" "$(pick_bin_path)"
fi

[ -n "$URL" ] || die "--url is required (or set NEEDLE_URL)"
[ -n "$TOKEN" ] || die "--token is required (or set NEEDLE_TOKEN)"

BIN="$(pick_bin_path)"
mkdir -p "$(dirname "$BIN")"

log "method: $METHOD"
log "binary: $BIN"

# Prefer a local build, then the network, then whatever is already installed.
# A download lands straight at $BIN; a local copy goes through a temp file
# beside it so the swap is atomic.
if SRC="$(local_binary)"; then
  if [ "$SRC" != "$BIN" ]; then
    tmp="$BIN.tmp.$$"
    cp "$SRC" "$tmp"
    chmod 755 "$tmp"
    mv -f "$tmp" "$BIN"
    log "installed from local $SRC"
  fi
elif download_binary "$BIN"; then
  chmod 755 "$BIN"
else
  warn "could not download $VERSION; falling back to an already installed agent"
  installed_binary >/dev/null || die "no $BIN_NAME available: download failed and none is installed"
  BIN="$(installed_binary)"
fi

case "$METHOD" in
  systemd-user) install_systemd_user "$BIN" ;;
  procd) install_procd "$BIN" ;;
  tmux) install_tmux "$BIN" ;;
  nohup) install_nohup "$BIN" ;;
  *) die "unknown method: $METHOD" ;;
esac

log "done. the agent reports to $URL every $INTERVAL"
if [ "$VERBOSE" = 1 ]; then
  "$BIN" --url "$URL" --token "$TOKEN" --once -v || true
fi
