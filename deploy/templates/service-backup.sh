#!/bin/bash
# 模板: 生成每个服务目录里的 backup.sh   (占位符: @@SVC@@ @@VOLUME_SKIP@@ @@VOLUME_EXCLUDES@@)
#
# 只备份本服务的 docker 命名卷, 不备份配置/脚本/compose 文件。
# 输出: <svc>/.volumes/<日期>/<卷名>.tar.gz —— 一次运行一个日期目录, 每个卷一个文件,
# 便于日后直接用 rsync 把 .volumes/ 传到别的机器。
#
# 卷数据经一次性容器 (官方 alpine, 内含 busybox tar/sh) 以容器内 root 读取
# (宿主 typist 属 docker 组即可), 无需 sudo, 也无需自建镜像。
# sqlite 一致性在宿主机侧用 sqlite3 .backup 重写 (库从容器管道出来, 落到 stage 后属 typist)。
#
# 脚本独立自包含, 用相对路径定位, 不依赖用户名/家目录, 可整目录搬走。
set -euo pipefail

SVC="@@SVC@@"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEST="$HERE/.volumes"
KEEP=7                                   # 保留最近几个日期目录
HELPER_IMG="alpine:3.20"                  # 直接用官方 alpine (busybox tar/sh 即可), 无需自建镜像
# 整个跳过的命名卷 (体积大或可重建)
VOLUME_SKIP=( @@VOLUME_SKIP@@ )
# 卷内不打包的相对路径 (相对各卷根)
VOLUME_EXCLUDES=( @@VOLUME_EXCLUDES@@ )

DRY=0
[ "${1:-}" = "--dry-run" ] && DRY=1

log()  { printf '\033[36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m[fail]\033[0m %s\n' "$*" >&2; exit 1; }

# 无需 root: 只要宿主用户能访问 docker socket 即可 (helper 容器内是 root)
docker info >/dev/null 2>&1 || die "无法访问 docker (需要当前用户在 docker 组)"

# 经一次性容器以 root 读取一个卷 -> stdout (由调用方接收)
# 镜像缺省时 docker 会自动拉取 alpine。
helper_tar() { # vol destincludes...
  local vol="$1"; shift
  local args=()
  for e in "$@"; do args+=(--exclude="./$e"); done
  docker run --rm -v "$vol":/vol:ro "$HELPER_IMG" tar -C /vol -cf - ${args[@]+"${args[@]}"} .
}

SKIP() { # 卷是否在 VOLUME_SKIP 里
  local v="$1" s
  for s in ${VOLUME_SKIP[@]+"${VOLUME_SKIP[@]}"}; do [ "$s" = "$v" ] && return 0; done
  return 1
}

DATE="$(date +%F)"
OUTDIR="$DEST/$DATE"

# 本服务的卷: 按 compose 项目标签 + 卷名前缀兜底 (标签缺失的旧卷也能覆盖)
service_volumes() {
  { docker volume ls -q --filter "label=com.docker.compose.project=$SVC" 2>/dev/null
    docker volume ls -q --filter "name=^${SVC}_" 2>/dev/null
  } | sort -u
}

# --------------------------------------------------------------------------
# 预演: 只列出将备份的卷
# --------------------------------------------------------------------------
if [ "$DRY" = 1 ]; then
  mapfile -t vols < <(service_volumes)
  log "dry-run: 输出目录 $OUTDIR"
  if [ "${#vols[@]}" -eq 0 ]; then
    warn "未发现命名卷 —— 本服务没有 docker 卷可备份"
    exit 0
  fi
  for v in "${vols[@]}"; do
    if SKIP "$v"; then
      warn "卷 $v: 在跳过清单, 忽略 -> $OUTDIR/$v.tar.gz"
    else
      log "卷 $v -> $OUTDIR/$v.tar.gz"
    fi
  done
  exit 0
fi

mkdir -p "$OUTDIR"

# --------------------------------------------------------------------------
# 逐个卷: 经 helper 读取 -> stage -> sqlite 一致化 -> 单独打包成 <卷名>.tar.gz
# --------------------------------------------------------------------------
mapfile -t vols < <(service_volumes)
[ "${#vols[@]}" -gt 0 ] || warn "未发现命名卷, 无内容可备份"

for v in ${vols[@]+"${vols[@]}"}; do
  if SKIP "$v"; then warn "卷 $v: 在跳过清单, 忽略"; continue; fi
  log "卷 $v -> $OUTDIR/$v.tar.gz"

  STAGE="$(mktemp -d "$DEST/.stage.$SVC.XXXXXX")"
  if ! helper_tar "$v" ${VOLUME_EXCLUDES[@]+"${VOLUME_EXCLUDES[@]}"} \
       | tar -C "$STAGE" -xf -; then
    rm -rf "$STAGE"
    die "卷 $v 读取失败 (helper 容器)"
  fi

  # sqlite 一致性: 用 .backup 重写, 丢弃 -wal / -shm
  if command -v sqlite3 >/dev/null 2>&1; then
    while IFS= read -r -d '' f; do
      if sqlite3 "$f" ".backup \"$f.__bak\"" 2>/dev/null; then
        mv -f "$f.__bak" "$f"
        rm -f "$f-wal" "$f-shm"
        log "  sqlite 一致快照: ${f#"$STAGE"/}"
      else
        rm -f "$f.__bak"
        warn "  非 sqlite, 原样保留: ${f#"$STAGE"/}"
      fi
    done < <(find "$STAGE" -type f \
               \( -name '*.db' -o -name '*.sqlite' -o -name '*.sqlite3' \) -print0)
  else
    warn "宿主机无 sqlite3, 跳过一致性处理 (库可能撕裂)"
  fi

  OUT="$OUTDIR/$v.tar.gz"
  tar -C "$STAGE" -czf "$OUT.tmp" . && mv -f "$OUT.tmp" "$OUT"
  rm -rf "$STAGE"
  log "  完成: $(du -h "$OUT" | cut -f1)"
done

# --------------------------------------------------------------------------
# 保留最近 $KEEP 个日期目录
# --------------------------------------------------------------------------
if [ -d "$DEST" ]; then
  mapfile -t old < <(find "$DEST" -mindepth 1 -maxdepth 1 -type d -name '[0-9][0-9][0-9][0-9]-*' -printf '%f\n' 2>/dev/null | sort | head -n "-$KEEP")
  for o in ${old[@]+"${old[@]}"}; do
    log "清理旧目录 $o/"
    rm -rf "$DEST/$o"
  done
fi

log "完成: $SVC 备份在 $OUTDIR/"
