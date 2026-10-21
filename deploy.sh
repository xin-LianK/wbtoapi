#!/usr/bin/env bash
# workbuddy2api-panel 本地部署脚本（Git Bash / Linux / macOS / 飞牛 NAS 通用）
#
#   ./deploy.sh           编译 + 起容器
#   ./deploy.sh rebuild    不重新编译，只重建容器
#   ./deploy.sh down       停止并删除容器
#   ./deploy.sh native     本机直接跑（需要装 Go，调试用）
#   ./deploy.sh key        打印当前 api_key
#   ./deploy.sh headers    打印实际出站 chat 请求头（dry-run，不连上游）
#   ./deploy.sh shape      上游形态快照：解包提取 vs 仓库基线（有漂移退出码 1）
#   ./deploy.sh watch      每日入口：客户端版本没变就跳过
#   ./deploy.sh stats      请求指标简报：昨日失败率/403/429 及前日环比（恶化退出码 1）
set -euo pipefail
cd "$(dirname "$0")"

MODE="${1:-up}"
COMPOSE_FILE="docker-compose.local.yml"

log()  { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[33mWARN: %s\033[0m\n' "$*"; }
die()  { printf '\033[31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }

have() { command -v "$1" >/dev/null 2>&1; }

# python 解析：PYTHON 环境变量优先，其次 python3/python/py 启动器。
# 不要把 PY 设成自定义函数名——那样 "$PY" 会调到函数而不是解释器。
PY="${PYTHON:-}"
if [ -z "$PY" ]; then
  for c in python3 python py; do
    if have "$c"; then PY="$c"; break; fi
  done
fi
rand_hex() {
  if have openssl; then openssl rand -hex 24
  else date +%s%N | sha256sum | cut -c1-48; fi
}

# ---------------------------------------------------------------- 配置准备
ensure_config() {
  if [ -f config.json ]; then
    log "config.json 已存在，跳过生成"
    return
  fi
  [ -f config.example.json ] || die "缺少 config.example.json"
  local key; key="$(rand_hex)"
  # 注意：这里只替换 api_key，其余字段沿用 example 的推荐值（定时任务全开、passthrough 提示词）
  sed "s/\"api_key\": *\"test_key\"/\"api_key\": \"$key\"/" config.example.json > config.json
  grep -q "$key" config.json || die "api_key 写入失败：config.example.json 的 api_key 字段可能已改名，请手工检查"
  chmod 600 config.json 2>/dev/null || true
  log "已生成 config.json（api_key 已随机化）"
}

ensure_dirs() {
  mkdir -p auths data dist/linux-amd64
}

# ---------------------------------------------------------------- 用户映射
# 绑定挂载目录属主与容器内 uid 不一致时，刷新 token 写回会 permission denied。
resolve_uid() {
  case "$(uname -s)" in
    MINGW*|MSYS*|CYGWIN*)
      # Windows Docker Desktop：bind mount 不映射 uid，用 root 最省事（仅容器内提权）
      export PUID="${PUID:-0}"; export PGID="${PGID:-0}";;
    *)
      export PUID="${PUID:-$(id -u)}"; export PGID="${PGID:-$(id -g)}";;
  esac
}

# ---------------------------------------------------------------- 编译
build_linux() {
  have go || die "未找到 go（需要 Go >= 1.22）"
  log "编译 linux/amd64 二进制到 dist/linux-amd64"
  local pair pkg out
  for pair in "server:wb2api" "signin:signin_bin" "login:login" "credit:credit"; do
    pkg="${pair%%:*}"; out="${pair##*:}"
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
      go build -trimpath -ldflags="-s -w" -o "dist/linux-amd64/$out" "./cmd/$pkg"
    printf '  %-12s %s\n' "$out" "$(du -h "dist/linux-amd64/$out" | cut -f1)"
  done
}

# ---------------------------------------------------------------- 入口
case "$MODE" in
  down)
    docker compose -f "$COMPOSE_FILE" down
    log "已停止"
    ;;

  key)
    [ -f config.json ] || die "config.json 不存在"
    sed -n 's/.*"api_key": *"\([^"]*\)".*/\1/p' config.json | head -1
    ;;

  headers)
    have go || die "未找到 go"
    shift
    go run ./cmd/headers-dump "$@"
    ;;

  shape)
    # 上游形态快照：解包提取为主、抓包导入为辅，同一份 schema 做跨版本 diff。
    # 不连上游、不碰账号，只读本机客户端产物。
    [ -n "$PY" ] || die "未找到 python"
    shift
    "$PY" tools/upstream-shape.py "$@"
    ;;

  watch)
    # 每日入口：客户端版本没变就一行退出（毫秒级），变了才跑完整对比。
    [ -n "$PY" ] || die "未找到 python"
    "$PY" tools/upstream-shape.py --watch
    ;;

  stats)
    # 请求指标简报：昨日 status / outcome 分布 + 前日环比（读 data/request-logs）。
    # 只读本地 JSONL，不连上游。退出码 1 = 指标显著恶化。
    [ -n "$PY" ] || die "未找到 python"
    shift
    "$PY" tools/req-stats.py "$@"
    ;;

  native)
    ensure_config; ensure_dirs
    have go || die "未找到 go"
    log "编译本机二进制并前台运行（Ctrl+C 退出）"
    go build -o dist/wb2api-native ./cmd/server
    exec ./dist/wb2api-native -config config.json
    ;;

  rebuild)
    ensure_config; ensure_dirs; resolve_uid
    log "重建容器（不重新编译）"
    docker compose -f "$COMPOSE_FILE" up -d --build
    docker compose -f "$COMPOSE_FILE" ps
    ;;

  up|"")
    ensure_config; ensure_dirs; resolve_uid
    build_linux
    have docker || die "未找到 docker"
    docker info >/dev/null 2>&1 || die "Docker 未运行，请先启动 Docker Desktop"
    log "构建镜像并启动容器（PUID=$PUID PGID=$PGID）"
    docker compose -f "$COMPOSE_FILE" up -d --build
    log "等待容器健康检查"
    for i in $(seq 1 40); do
      hs="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' workbuddy2api 2>/dev/null || echo none)"
      if [ "$hs" = "healthy" ]; then log "容器健康检查通过"; break; fi
      sleep 1
      if [ "$i" = 40 ]; then warn "40s 内未 healthy（当前 ${hs}），查看：docker logs workbuddy2api --tail 50"; fi
    done

    port="${WB2A_PORT:-7863}"
    ready="$(docker exec workbuddy2api wget -qO- "http://127.0.0.1:7863/healthz" 2>/dev/null || true)"
    if printf '%s' "$ready" | grep -q '"healthy":0'; then
      warn "当前没有可服务账号（/healthz 返回 503 属正常初装状态），请先去面板添加账号"
    fi

    cat <<EOF

面板      http://127.0.0.1:${port}/panel/
API 基址  http://127.0.0.1:${port}/v1
API Key   $(sed -n 's/.*"api_key": *"\([^"]*\)".*/\1/p' config.json | head -1)

下一步：
  1. 打开面板 → 添加账号 → 扫码/浏览器授权
  2. 凭证落在 ./auths/workbuddy-<uid>.json（明文 token，勿提交、勿外传）
  3. curl 验证：
     curl -s http://127.0.0.1:${port}/v1/chat/completions \\
       -H "Authorization: Bearer \$(./deploy.sh key)" \\
       -H "Content-Type: application/json" \\
       -d '{"model":"<面板里显示的模型名>","messages":[{"role":"user","content":"hi"}]}'
EOF
    ;;

  *)
    sed -n '2,9p' "$0"
    exit 1
    ;;
esac
