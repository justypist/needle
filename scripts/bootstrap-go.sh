#!/bin/bash
# 下载项目自带的 Go 工具链到 .toolchain/ (幂等)
set -euo pipefail

GO_VERSION="${GO_VERSION:-1.27.2}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ROOT="$HERE/.toolchain"
DEST="$ROOT/go"

[ -x "$DEST/bin/go" ] && [ "$("$DEST/bin/go" version 2>/dev/null | awk '{print $3}')" = "go$GO_VERSION" ] && {
  echo "go $GO_VERSION already at $DEST"; exit 0
}

case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported host arch: $(uname -m)" >&2; exit 1 ;;
esac

TARBALL="go${GO_VERSION}.linux-${ARCH}.tar.gz"
MIRRORS=(
  "https://golang.google.cn/dl/$TARBALL"
  "https://mirrors.aliyun.com/golang/$TARBALL"
  "https://go.dev/dl/$TARBALL"
)

mkdir -p "$ROOT"
TMP="$(mktemp -d "$ROOT/.dl.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

echo "downloading $TARBALL ..."
ok=0
for u in "${MIRRORS[@]}"; do
  if curl -fSL --retry 3 --connect-timeout 15 -o "$TMP/$TARBALL" "$u"; then ok=1; break; fi
  echo "  mirror failed: $u" >&2
done
[ "$ok" = 1 ] || { echo "all mirrors failed" >&2; exit 1; }

rm -rf "$DEST"
tar -C "$ROOT" -xzf "$TMP/$TARBALL"
"$DEST/bin/go" version
