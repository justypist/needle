#!/bin/bash
# Assemble a needle-server deployment directory.
#
# The image comes from GHCR, built and published by GitHub Actions, so this
# host needs no source tree, no Go and no local build. What lands in the
# destination is a compose root that follows the ~/.compose convention:
# compose.yaml, .env, and the backup.sh / restore.sh pair.
#
#   ./install-server.sh --dest ~/.compose/needle --domain needle.dev.typist.cc
#
# Re-run it to update the compose file; the existing .env is preserved.
set -euo pipefail

SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEST="${NEEDLE_DEST:-}"
DOMAIN="${NEEDLE_DOMAIN:-}"
TOKEN="${NEEDLE_AGENT_TOKEN:-}"
PASSWORD="${NEEDLE_PASSWORD:-}"
SESSION_SECRET="${NEEDLE_SESSION_SECRET:-}"
NETWORK="${NEEDLE_NETWORK:-public}"
TAG="${NEEDLE_TAG:-latest}"
START=1

log()  { printf '\033[36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m[fail]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
usage: install-server.sh --dest <dir> --domain <host> [options]

  --dest <dir>       deployment directory to assemble (required)
  --domain <host>    public hostname, e.g. needle.dev.typist.cc (required)
  --token <token>    agent token; generated when omitted
  --password <pw>    dashboard password; prompted when omitted
  --tag <tag>        image tag to deploy (default latest)
  --network <name>   docker network caddy watches (default public)
  --no-start         assemble only, do not run docker compose up
  -h, --help         this message

If <dest> already holds a deployment the existing .env is kept, so re-running
this does not rotate credentials.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dest) DEST="${2:-}"; shift 2 ;;
    --domain) DOMAIN="${2:-}"; shift 2 ;;
    --token) TOKEN="${2:-}"; shift 2 ;;
    --password) PASSWORD="${2:-}"; shift 2 ;;
    --tag) TAG="${2:-}"; shift 2 ;;
    --network) NETWORK="${2:-}"; shift 2 ;;
    --no-start) START=0; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1 (try --help)" ;;
  esac
done

[ -n "$DEST" ] || die "--dest is required"
[ -n "$DOMAIN" ] || die "--domain is required"
command -v docker >/dev/null 2>&1 || die "docker not found"

mkdir -p "$DEST"

# --------------------------------------------------------------------------
# 1) compose file, with the network name substituted in
# --------------------------------------------------------------------------
sed "s|^      - public$|      - $NETWORK|; s|^  public:$|  $NETWORK:|" \
  "$SRC/deploy/compose.yaml" > "$DEST/compose.yaml"
log "wrote $DEST/compose.yaml (image tag: $TAG)"

# --------------------------------------------------------------------------
# 2) credentials: keep whatever is already there
# --------------------------------------------------------------------------
ENV_FILE="$DEST/.env"
if [ -f "$ENV_FILE" ]; then
  log "keeping existing .env"
  TOKEN="$(sed -n 's/^NEEDLE_AGENT_TOKEN=//p' "$ENV_FILE" | head -1)"
  PASSWORD="$(sed -n 's/^NEEDLE_PASSWORD=//p' "$ENV_FILE" | head -1)"
  SESSION_SECRET="$(sed -n 's/^NEEDLE_SESSION_SECRET=//p' "$ENV_FILE" | head -1)"
  DOMAIN="$(sed -n 's/^NEEDLE_DOMAIN=//p' "$ENV_FILE" | head -1)"
  TAG="$(sed -n 's/^NEEDLE_TAG=//p' "$ENV_FILE" | head -1)"
  [ -n "$TAG" ] || TAG=latest
else
  if [ -z "$TOKEN" ]; then
    TOKEN="$(openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
    log "generated an agent token"
  fi
  if [ -z "$PASSWORD" ]; then
    if [ -t 0 ]; then
      read -r -s -p "dashboard password: " PASSWORD
      echo
    fi
    [ -n "$PASSWORD" ] || PASSWORD="$(openssl rand -hex 12 2>/dev/null || head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')"
    log "dashboard password: $PASSWORD"
  fi
  if [ -z "$SESSION_SECRET" ]; then
    SESSION_SECRET="$(openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  fi
  umask 077
  cat > "$ENV_FILE" <<EOF
NEEDLE_DOMAIN=$DOMAIN
NEEDLE_TAG=$TAG
NEEDLE_AGENT_TOKEN=$TOKEN
NEEDLE_PASSWORD=$PASSWORD
NEEDLE_SESSION_SECRET=$SESSION_SECRET
EOF
  log "wrote $ENV_FILE"
fi

# --------------------------------------------------------------------------
# 3) backup/restore, following the ~/.compose convention
# --------------------------------------------------------------------------
SVC="$(basename "$DEST")"
for pair in "service-backup.sh:backup.sh" "service-restore.sh:restore.sh"; do
  tmpl="${pair%%:*}"; out="${pair##*:}"
  # The canonical templates live in ~/.compose/.backup; fall back to the copy
  # shipped with this repo when installing somewhere else.
  src=""
  for cand in "$HOME/.compose/.backup/$tmpl" "$SRC/deploy/templates/$tmpl"; do
    [ -f "$cand" ] && { src="$cand"; break; }
  done
  [ -n "$src" ] || continue
  sed "s|@@SVC@@|$SVC|g; s|@@VOLUME_SKIP@@||g; s|@@VOLUME_EXCLUDES@@||g" "$src" > "$DEST/$out"
  chmod +x "$DEST/$out"
done

log "deployment assembled at $DEST"

# --------------------------------------------------------------------------
# 4) pull and start
# --------------------------------------------------------------------------
if [ "$START" = 1 ]; then
  log "pulling and starting"
  ( cd "$DEST" && docker compose pull --quiet && docker compose up -d )
  log "server up. point an agent at it with:"
  echo "    ./install.sh --url https://$DOMAIN --token $TOKEN"
else
  log "skipped start (--no-start); run: cd $DEST && docker compose up -d"
fi
