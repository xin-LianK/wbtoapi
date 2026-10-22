"""mitmproxy 插件：把命中上游主机的请求压成 JSONL（头 + body 前 16KB）。

配合 tools/capture-upstream.sh 使用。只记录业务主机，避免被遥测/本地回环淹没。

写法说明（踩过的坑，别改回去）
================================

1. 用**模块级函数** hook，不用 class addon。实测 mitmproxy 12.2.3 在 Windows
   装成 exe 时，class 形式（class UpstreamDump: def response(self, flow)）模块
   能被 import（模块级代码会执行），但实例的 hook 从不触发、也不报错——
   mitmproxy 在无 tty 的环境下把加载期问题一并吞掉，表现就是"抓到 0 条、
   没有异常、没有日志"。模块级函数 hook 是各版本都认的最小公分母。

2. **分三个阶段写，绝不在 response() 里读 body**。这是第二口坑：
   chat 是 SSE（text/event-stream）长连接，响应永不结束。mitmproxy 默认会把
   response body 完整缓冲后才触发 response 事件，于是 response() 永远等不到，
   连先写的 .hit 都不落盘。表现就是"客户端明明在发消息，jsonl 一动不动，
   但 netstat 能看到 ESTABLISHED 连接持续增加"——连接数涨、记录不涨。
   所以：
     - request(flow)           → 立刻写请求（头+请求体）。chat 的核心信息
                                 （body 键集、UA、trace id、头顺序）全在这里。
     - responseheaders(flow)   → 写状态码+响应头；**仅当 content-type 是
                                 text/event-stream 时**才 flow.response.stream=True。
                                 无差别 stream 会把普通 JSON 响应体全丢掉
                                 （config 下发内容为空就是这么来的）。
     - response(flow)          → 非 SSE 响应在这里读 body（JSON 不大，安全）。
     - error(flow)             → 记录代理侧失败（TLS/连接错误），别静默。
"""
import json
import os
import re
import time
import traceback

OUT = os.environ.get("MITM_OUT", "capture/upstream.jsonl")
HOST_RE = os.environ.get("MITM_HOST_RE", r"tencent\.com|copilot|workbuddy|codebuddy")
BODY_LIMIT = int(os.environ.get("MITM_BODY_LIMIT", "16000"))

os.makedirs(os.path.dirname(OUT) or ".", exist_ok=True)
_host = re.compile(HOST_RE)


def _mark(suffix, text):
    """自证用的旁路标记：正式输出旁边写一个文件，失败不影响抓包。"""
    try:
        with open(OUT + suffix, "w", encoding="utf-8") as f:
            f.write(text)
    except Exception:
        pass


def _append(suffix, line):
    try:
        with open(OUT + suffix, "a", encoding="utf-8") as f:
            f.write(line + "\n")
    except Exception:
        pass


def _write(rec):
    try:
        with open(OUT, "a", encoding="utf-8") as f:
            f.write(json.dumps(rec, ensure_ascii=False) + "\n")
    except Exception:
        _append(".err", traceback.format_exc())


# import 时就落一个标记：没有它说明 addon 根本没被加载（-s 没生效/路径错/
# 端口被别的代理进程占着），和"客户端没走代理"是两码事。
_mark(".imported", f"{time.time()} pid={os.getpid()} host_re={HOST_RE}\n")


def _matched(host):
    return bool(_host.search(host))


def request(flow) -> None:
    """请求阶段：不等响应，立刻落盘。SSE 也拦不住这一条。"""
    host = flow.request.pretty_host
    # 全量旁路：所有经过代理的主机都记一行（不管匹不匹配），
    # 用来区分「chat 没走代理」和「chat 走了但 host_re 写窄了」。
    _append(".hosts", f"{time.time()} {flow.request.method} {flow.request.scheme}://{host}{flow.request.path}")
    if not _matched(host):
        return
    _append(".hit", f"{time.time()} {host}{flow.request.path}")
    try:
        req = flow.request
        # 流式上传的请求体不碰，避免阻塞；本项目的上游请求都是普通 JSON body。
        if getattr(req, "stream", False):
            body = b""
            body_len = -1
        else:
            body = req.content or b""
            body_len = len(body)
        _write({
            "kind": "req",
            "ts": int(time.time()),
            "host": host,
            "method": req.method,
            "scheme": req.scheme,
            "path": req.path.split("?")[0],
            "query_keys": list(req.query.keys()) if req.query else [],
            "http_version": req.http_version,
            # 保留原始顺序与大小写：抓包的价值之一就是核对这两点
            "headers": [[k, v] for k, v in req.headers.items()],
            "body_len": body_len,
            "body": body[:BODY_LIMIT].decode("utf-8", "replace"),
        })
    except Exception:
        _append(".err", traceback.format_exc())


def responseheaders(flow) -> None:
    """响应头阶段：记元信息；只有 SSE 才关缓冲，普通 JSON 照常缓冲取 body。"""
    try:
        if not _matched(flow.request.pretty_host):
            return
        resp = flow.response
        if resp is None:
            return
        ctype = (resp.headers.get("content-type") or "").lower()
        is_sse = "text/event-stream" in ctype
        _write({
            "kind": "resp_head",
            "ts": int(time.time()),
            "host": flow.request.pretty_host,
            "method": flow.request.method,
            "path": flow.request.path.split("?")[0],
            "status": resp.status_code,
            "headers": [[k, v] for k, v in resp.headers.items()],
            "sse": is_sse,
        })
        # 只有 SSE 才关缓冲：它永不结束，缓冲会让这条 flow 一直挂着，
        # 把后续请求一起拖死。反过来，无差别 stream 会让所有普通 JSON
        # 响应体永远拿不到（早期版本的坑：config 下发内容全是空）。
        if is_sse:
            resp.stream = True
    except Exception:
        _append(".err", traceback.format_exc())


def response(flow) -> None:
    """非 SSE 响应：这里才读 body（config 下发的端点/开关全在响应里）。"""
    try:
        if not _matched(flow.request.pretty_host):
            return
        resp = flow.response
        if resp is None:
            return
        ctype = (resp.headers.get("content-type") or "").lower()
        if "text/event-stream" in ctype:
            return  # 流式已在 responseheaders 里放行，body 不落盘
        raw = resp.content or b""
        _write({
            "kind": "resp",
            "ts": int(time.time()),
            "host": flow.request.pretty_host,
            "method": flow.request.method,
            "path": flow.request.path.split("?")[0],
            "status": resp.status_code,
            "headers": [[k, v] for k, v in resp.headers.items()],
            "body_len": len(raw),
            "body": raw[:BODY_LIMIT].decode("utf-8", "replace"),
        })
    except Exception:
        _append(".err", traceback.format_exc())



def error(flow) -> None:
    """代理侧失败（TLS 握手、上游连接断等），记下来别静默。"""
    try:
        msg = getattr(flow, "error", None)
        _append(".err", f"{time.time()} ERROR {flow.request.pretty_host}{flow.request.path} {msg}")
    except Exception:
        pass
