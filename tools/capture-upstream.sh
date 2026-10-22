#!/usr/bin/env bash
# 抓上游真实请求：本地 TLS 中间人（mitmproxy）。
#
# 为什么必须重启客户端才能生效
# ----------------------------
# 客户端业务请求走 Node 的 https（axios + HttpsProxyAgent），TLS 校验用 **Node 内置
# CA 列表**。Windows 上 Node 不读系统根证书库；客户端代码里虽然定义了
# `--use-system-ca` 常量，但从未 appendSwitch，所以那条路是死的。
# 结论：只能用启动期的 NODE_EXTRA_CA_CERTS 注入，无法热生效。
# （代理本身是运行时解析的——Electron 的系统代理规则会被转成 HTTP_PROXY 再喂给
#   axios——所以只有"证书"这一项卡住。）
#
# 用法:
#   tools/capture-upstream.sh start     # 起代理，并打印重启客户端的命令
#   tools/capture-upstream.sh stop      # 停代理
#   tools/capture-upstream.sh status    # 看是否在跑、抓了多少条
#
# 抓完记得 stop，然后**不带环境变量**正常启动客户端，把代理摘掉。
set -euo pipefail
cd "$(dirname "$0")/.."

PORT="${PORT:-8888}"
CAPTURE_DIR="${CAPTURE_DIR:-capture}"
PIDFILE="$CAPTURE_DIR/mitm.pid"
OUT="$CAPTURE_DIR/upstream-$(date +%Y%m%d-%H%M%S).jsonl"
MITMDUMP="${MITMDUMP:-$HOME/.workbuddy/binaries/python/envs/mitm/Scripts/mitmdump.exe}"
CA="${MITM_CA:-$HOME/.mitmproxy/mitmproxy-ca-cert.pem}"
ADDON="tools/mitm-upstream-dump.py"

is_running() {
  [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null
}

case "${1:-start}" in
  start)
    [ -x "$MITMDUMP" ] || { echo "找不到 mitmdump：$MITMDUMP"; echo "装法：python -m venv ~/.workbuddy/binaries/python/envs/mitm && ~/.workbuddy/binaries/python/envs/mitm/Scripts/pip install mitmproxy"; exit 2; }
    [ -f "$CA" ] || { echo "找不到 MITM CA：$CA（先跑一次 mitmdump 生成）"; exit 2; }
    if is_running; then echo "已在运行（pid $(cat "$PIDFILE")），输出 $OUT"; exit 0; fi

    mkdir -p "$CAPTURE_DIR"
    MITM_OUT="$(pwd)/$OUT" nohup "$MITMDUMP" -p "$PORT" -q -s "$ADDON" >"$CAPTURE_DIR/mitm.log" 2>&1 &
    echo $! > "$PIDFILE"
    sleep 1
    is_running || { echo "启动失败，看 $CAPTURE_DIR/mitm.log"; exit 2; }

    cat <<EOF
代理已起：127.0.0.1:$PORT（pid $(cat "$PIDFILE")）
输出：$OUT

接下来说明两步操作：

1. 退出 WorkBuddy 客户端（会中断当前对话，重启后可继续）。

2. 带证书 + 代理环境变量重新启动。cmd 里执行：

   set NODE_EXTRA_CA_CERTS=$CA
   set HTTPS_PROXY=http://127.0.0.1:$PORT
   set HTTP_PROXY=http://127.0.0.1:$PORT
   set NO_PROXY=127.0.0.1,localhost
   start "" "C:\\Program Files\\WorkBuddy\\WorkBuddy.exe"

   Git Bash 里执行：

   NODE_EXTRA_CA_CERTS="$CA" HTTPS_PROXY=http://127.0.0.1:$PORT NO_PROXY=127.0.0.1,localhost \\
     "/c/Program Files/WorkBuddy/WorkBuddy.exe" &

3. 正常聊一句（发一条消息），请求就会落到 $OUT。
   然后 tools/capture-upstream.sh stop，再不带环境变量重启客户端恢复原状。
EOF
    ;;

  stop)
    if is_running; then kill "$(cat "$PIDFILE")" 2>/dev/null || true; rm -f "$PIDFILE"; echo "已停止"; else echo "没在跑"; fi
    ;;

  status)
    if is_running; then echo "运行中（pid $(cat "$PIDFILE")）"; else echo "未运行"; fi
    for f in "$CAPTURE_DIR"/upstream-*.jsonl; do
      [ -e "$f" ] || continue
      echo "  $f  $(wc -l < "$f") 条"
    done
    ;;

  *) echo "用法: $0 start|stop|status"; exit 2 ;;
esac
