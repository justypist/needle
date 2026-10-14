#!/bin/bash
# 模板: 生成每个服务目录里的 restore.sh   (占位符: @@SVC@@)
#
# 还原 docker 命名卷 (只还原卷, 不碰配置/脚本/compose 文件)。
# 备份包结构: <svc>/.volumes/<日期>/<卷名>.tar.gz
#
# 卷操作经一次性官方 alpine 容器 (busybox tar/sh) 以容器内 root 完成, 无需 sudo, 无需自建镜像。
set -euo pipefail

SVC="@@SVC@@"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEST="$HERE/.volumes"
HELPER_IMG="alpine:3.20"                  # 直接用官方 alpine (busybox tar/sh 即可), 无需自建镜像
COMPOSE="docker compose -f $HERE/compose.yaml"

log()  { printf '\033[36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m[fail]\033[0m %s\n' "$*" >&2; exit 1; }

docker info >/dev/null 2>&1 || die "无法访问 docker (需要当前用户在 docker 组)"

# 按日期倒序列出备份目录
backup_dirs() {
  find "$DEST" -mindepth 1 -maxdepth 1 -type d -name '[0-9][0-9][0-9][0-9]-*' -printf '%f\n' 2>/dev/null | sort -r
}

if [ "${1:-}" = "--list" ] || [ "${1:-}" = "-l" ]; then
  backup_dirs || true
  exit 0
fi

ARG="${1:-}"
if [ -z "$ARG" ]; then
  ARG="$(backup_dirs | head -1 || true)"
  [ -n "$ARG" ] || die "未指定备份目录, 且 $DEST 下无备份"
fi

# 允许直接给日期 (2026-10-01) 或完整路径
if [ -d "$ARG" ]; then
  SRCDIR="$ARG"
elif [ -d "$DEST/$ARG" ]; then
  SRCDIR="$DEST/$ARG"
else
  die "找不到备份目录: $ARG"
fi

mapfile -t tars < <(find "$SRCDIR" -maxdepth 1 -type f -name '*.tar.gz' | sort)
[ "${#tars[@]}" -gt 0 ] || die "$SRCDIR 下没有 *.tar.gz"

log "使用备份目录: $SRCDIR"
log "将还原下列卷:"
for t in "${tars[@]}"; do printf '    %s\n' "$(basename "$t")"; done

read -r -p "确认还原到本服务? [y/N] " ans
[ "$ans" = y ] || [ "$ans" = Y ] || die "已取消"

# 服务一旦被 down, 退出时 (成功或失败) 都要把它重新拉起 —— 卷无论灌好
# 还是留底都在, 起服务是安全的, 避免中途失败把服务留在停机状态。
DOWNED=0
cleanup() { [ "$DOWNED" = 1 ] && $COMPOSE up -d >/dev/null 2>&1 || true; }
trap cleanup EXIT

# --------------------------------------------------------------------------
# 1) 停服务 (compose down: 删除容器, 确保卷回灌后必然重建出新容器;
#    仅 stop 会保留旧容器, 卷内容由旧进程持有, 可能看到陈旧数据)
# --------------------------------------------------------------------------
log "停止服务 ($SVC)"
$COMPOSE down || warn "compose down 返回非零 (可能本无容器)"
DOWNED=1

# 让 compose 按自己的规范创建命名卷 (标签齐全: 项目/卷名/config-hash, 项目名
# 也由 compose 推导), 但不启动容器 —— 此时还没有进程占用卷, 可安全灌数据。
# 新环境重建时 compose 配置与 .env 随整份 ~/.compose 一起带过来, 能正常求值。
log "创建卷/容器定义 ($SVC)"
$COMPOSE create || warn "compose create 返回非零 (可能环境变量缺失); 下面回退为手动建卷"

# --------------------------------------------------------------------------
# 2) 卷数据: 经 helper 容器以 root 操作 (宿主 typist 无需 sudo)
#    绑定卷的宿主目录 /var/lib/docker/volumes/<v>, 于是 <v>/_data 是真实路径,
#    可把旧数据改名 _data.pre-restore-<ts> 留底, 再灌入备份。
# --------------------------------------------------------------------------
TS="$(date +%F-%H%M%S)"
for t in "${tars[@]}"; do
  v="$(basename "$t" .tar.gz)"
  # 兜底: 若 compose create 没能建出这个卷, 手建并打上项目标签 (避免 compose up
  # 报 "already exists but was not created by Docker Compose"); 已存在则幂等跳过。
  if ! docker volume inspect "$v" >/dev/null 2>&1; then
    docker volume create --label "com.docker.compose.project=$SVC" "$v" >/dev/null
  fi
  log "卷 $v: 留底旧数据 + 灌入备份"
  docker run --rm \
    -v "/var/lib/docker/volumes/$v":/vol \
    -v "$t":/backup.tar.gz:ro \
    -e TS="$TS" \
    "$HELPER_IMG" sh -c '
      set -e
      cd /vol
      if [ -d _data ]; then
        for old in _data.pre-restore-*; do [ -e "$old" ] && rm -rf "$old"; done
        mv _data "_data.pre-restore-$TS"
      fi
      mkdir -p _data
      tar -C _data -xzf /backup.tar.gz
    ' || die "卷 $v 还原失败"
done

# --------------------------------------------------------------------------
# 3) 启动
# --------------------------------------------------------------------------
log "启动服务 ($SVC)"
$COMPOSE up -d || die "compose up 失败"
STARTED=1
log "还原完成。旧卷数据保留在 <卷>/_data.pre-restore-$TS, 确认无误后可删。"
