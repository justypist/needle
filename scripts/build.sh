#!/bin/bash
# Build needle-server and needle-client.
#
# Uses the project-local Go toolchain (scripts/bootstrap-go.sh) so the host
# needs nothing but bash, curl and tar. Binaries land in dist/.
#
#   ./scripts/build.sh                 # host arch only
#   ./scripts/build.sh --all           # cross-compile every target
#   ./scripts/build.sh --target linux-arm64
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="$HERE/.toolchain/go/bin/go"
export PATH="$(dirname "$GO"):$PATH"
export CGO_ENABLED=0
export GOFLAGS="${GOFLAGS:--trimpath}"

: "${GOPROXY:=https://proxy.golang.org,direct}"
export GOPROXY

# The agent runs on every host, including the router.
AGENT_TARGETS=(
  linux-amd64
  linux-arm64
  linux-arm
  linux-mipsle
)

# The server embeds a SQLite engine whose 32-bit port is incomplete, so the
# server is only built for 64-bit targets. It is deployed as a container
# anyway; this list exists for local runs and for a bare-metal fallback.
SERVER_TARGETS=(
  linux-amd64
  linux-arm64
)

VERSION="${VERSION:-$(date +%Y.%m.%d)}"
LDFLAGS="-s -w -X main.version=$VERSION"

targets=()
mode=host
while [ $# -gt 0 ]; do
  case "$1" in
    --all) mode=all ;;
    --target) mode=explicit; shift; targets+=("$1") ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
  shift
done

[ -x "$GO" ] || "$HERE/scripts/bootstrap-go.sh"

log() { printf '\033[36m==>\033[0m %s\n' "$*"; }

if [ "$mode" = host ]; then
  case "$(uname -m)" in
    x86_64) targets=(linux-amd64) ;;
    aarch64|arm64) targets=(linux-arm64) ;;
    *) echo "unsupported host arch $(uname -m); pass --target" >&2; exit 1 ;;
  esac
elif [ "$mode" = all ]; then
  targets=("${AGENT_TARGETS[@]}" "${SERVER_TARGETS[@]}")
fi

# Does this target get the server too, or only the agent?
serves() {
  local t="$1" s
  for s in "${SERVER_TARGETS[@]}"; do [ "$s" = "$t" ] && return 0; done
  return 1
}

log "go $("$GO" version | awk '{print $3}')  version=$VERSION  targets=${targets[*]}"

# Format and vet once, against the host toolchain, rather than per target.
if [ "$mode" != explicit ]; then
  unformatted="$(gofmt -l "$HERE/cmd" "$HERE/internal" "$HERE/embed.go" 2>/dev/null || true)"
  if [ -n "$unformatted" ]; then
    echo "unformatted files:" >&2
    echo "$unformatted" >&2
    exit 1
  fi
  log "vet"
  (cd "$HERE" && "$GO" vet ./...)
fi

mkdir -p "$HERE/dist"

for t in "${targets[@]}"; do
  os="${t%%-*}"
  arch="${t#*-}"
  cmds=(needle-client)
  serves "$t" && cmds=(needle-server needle-client)
  for cmd in "${cmds[@]}"; do
    out="$HERE/dist/${cmd}-${os}-${arch}"
    log "build $cmd -> dist/$(basename "$out")"
    (cd "$HERE" && GOOS="$os" GOARCH="$arch" "$GO" build -ldflags "$LDFLAGS" -o "$out" "./cmd/$cmd")
  done
done

log "done"
ls -lh "$HERE/dist" | tail -n +2 | awk '{printf "  %-34s %s\n", $9, $5}'
