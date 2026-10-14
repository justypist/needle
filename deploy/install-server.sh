#!/bin/sh
# needle-server deployment installer.
#
# Assembles a deployment directory from the files published in the repository
# and starts it: compose.yaml, the backup/restore pair, and a .env with fresh
# credentials. Needs only curl or wget and docker; no repository checkout, no
# Go, no build. The server image itself comes from GHCR.
#
#   curl -fsSL https://raw.githubusercontent.com/justypist/needle/main/deploy/install-server.sh \
#     | sh -s -- --dest ~/.compose/needle --domain needle.dev.typist.cc
#
# Re-running it refreshes compose.yaml and keeps the existing .env, so it
# doubles as the update path.
set -eu

REPO="${NEEDLE_REPO:-justypist/needle}"
REF="${NEEDLE_REF:-main}"
RAW_BASE="${NEEDLE_RAW_BASE:-}"

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

have() { command -v "$1" >/dev/null 2>&1; }

fetch() { # url dest
  if have curl; then
    curl -fsSL --retry 3 --connect-timeout 15 -o "$2" "$1"
  elif have wget; then
    wget -q -O "$2" "$1"
  else
    die "neither curl nor wget is available"
  fi
}

usage() {
  cat <<'EOF'
usage: install-server.sh --dest <dir> --domain <host> [options]

  --dest <dir>       deployment directory to assemble (required)
  --domain <host>    public hostname, e.g. needle.dev.typist.cc (required)
  --token <token>    agent token; generated when omitted
  --password <pw>    dashboard password; prompted when omitted
  --tag <tag>        image tag to deploy: latest, or a version like 1.2.3
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
have docker || die "docker not found"

# A local checkout wins, so this script also works from a clone. ROOT is the
# repository root when there is one; the paths below are all relative to it,
# which is also how they appear in the published URLs.
ROOT=""
case "${0:-}" in
  ""|-|sh|dash|bash) ;;
  *)
    if [ -f "$0" ]; then
      ROOT="$(cd "$(dirname "$0")/.." && pwd)"
      [ -d "$ROOT/deploy" ] || ROOT=""
    fi
    ;;
esac

# grab writes a published file to $2, preferring a local checkout.
grab() { # repo-relative path dest
  local rel="$1" dest="$2"
  if [ -n "$ROOT" ] && [ -f "$ROOT/$rel" ]; then
    cp "$ROOT/$rel" "$dest"
    return 0
  fi
  local url
  if [ -n "$RAW_BASE" ]; then
    url="${RAW_BASE%/}/$rel"
  else
    url="https://raw.githubusercontent.com/$REPO/$REF/$rel"
  fi
  fetch "$url" "$dest"
}

mkdir -p "$DEST"

# --------------------------------------------------------------------------
# 1) compose file and the backup/restore pair
# --------------------------------------------------------------------------
grab deploy/compose.yaml "$DEST/compose.yaml.part" || die "could not fetch compose.yaml"
# The published compose file hardcodes the upstream network name; rewrite it
# so --network works without shipping a templated copy.
sed "s|^      - public\$|      - $NETWORK|; s|^  public:\$|  $NETWORK:|" \
  "$DEST/compose.yaml.part" > "$DEST/compose.yaml"
rm -f "$DEST/compose.yaml.part"
log "wrote $DEST/compose.yaml (image tag: $TAG)"

SVC="$(basename "$DEST")"
for pair in "service-backup.sh:backup.sh" "service-restore.sh:restore.sh"; do
  tmpl="${pair%%:*}"; out="${pair##*:}"
  if grab "deploy/templates/$tmpl" "$DEST/$out.part"; then
    sed "s|@@SVC@@|$SVC|g; s|@@VOLUME_SKIP@@||g; s|@@VOLUME_EXCLUDES@@||g" \
      "$DEST/$out.part" > "$DEST/$out"
    rm -f "$DEST/$out.part"
    chmod +x "$DEST/$out"
  else
    warn "could not fetch $tmpl; skipping $out"
  fi
done

# --------------------------------------------------------------------------
# 2) credentials: keep whatever is already there
# --------------------------------------------------------------------------
ENV_FILE="$DEST/.env"
random_hex() { # bytes
  if have openssl; then
    openssl rand -hex "$1"
  else
    head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

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
    TOKEN="$(random_hex 32)"
    log "generated an agent token"
  fi
  if [ -z "$PASSWORD" ]; then
    if [ -t 0 ]; then
      printf 'dashboard password: '
      read -r PASSWORD
    fi
    [ -n "$PASSWORD" ] || PASSWORD="$(random_hex 12)"
    log "dashboard password: $PASSWORD"
  fi
  [ -n "$SESSION_SECRET" ] || SESSION_SECRET="$(random_hex 32)"
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

log "deployment assembled at $DEST"

# --------------------------------------------------------------------------
# 3) pull and start
# --------------------------------------------------------------------------
if [ "$START" = 1 ]; then
  log "pulling and starting"
  ( cd "$DEST" && docker compose pull --quiet && docker compose up -d )
  log "server up. point an agent at it with:"
  echo "    curl -fsSL https://raw.githubusercontent.com/$REPO/$REF/deploy/client/install.sh \\"
  echo "      | sh -s -- --url https://$DOMAIN --token $TOKEN"
else
  log "skipped start (--no-start); run: cd $DEST && docker compose up -d"
fi
