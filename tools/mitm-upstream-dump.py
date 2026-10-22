"""mitmproxy 插件：把命中上游主机的请求压成 JSONL（头 + body 前 16KB）。

配合 tools/capture-upstream.sh 使用。只记录业务主机，避免被遥测/本地回环淹没。

写法说明（踩过的坑，别改回去）
--------------------------------
用**模块级函数** hook，不用 class addon。实测 mitmproxy 12.2.3 在 Windows
装成 exe 时，class 形式（class UpstreamDump: def response(self, flow)）模块
能被 import（模块级代码会执行），但实例的 hook 从不触发、也不报错——
mitmproxy 在无 tty 的环境下把加载期问题一并吞掉，表现就是"抓到 0 条、
没有异常、没有日志"。模块级函数 hook 是各版本都认的最小公分母。
"""
import json
import os
import re
import time

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


# import 时就落一个标记：没有它说明 addon 根本没被加载（-s 没生效/路径错/
# 端口被别的代理进程占着），和"客户端没走代理"是两码事。
_mark(".imported", f"{time.time()} pid={os.getpid()} host_re={HOST_RE}\n")


def _dump(flow):
    host = flow.request.pretty_host
    if not _host.search(host):
        return
    req = flow.request
    rec = {
        "ts": int(time.time()),
        "host": host,
        "method": req.method,
        "scheme": req.scheme,
        "path": req.path.split("?")[0],
        "query_keys": list(req.query.keys()) if req.query else [],
        "http_version": req.http_version,
        "status": flow.response.status_code if flow.response else None,
        # 保留原始顺序与大小写：抓包的价值之一就是核对这两点
        "headers": [[k, v] for k, v in req.headers.items()],
        "body_len": len(req.content or b""),
        "body": (req.content or b"")[:BODY_LIMIT].decode("utf-8", "replace"),
    }
    with open(OUT, "a", encoding="utf-8") as f:
        f.write(json.dumps(rec, ensure_ascii=False) + "\n")


def response(flow) -> None:
    with open(OUT + ".hit", "a", encoding="utf-8") as f:
        f.write(f"{time.time()} {flow.request.pretty_host}{flow.request.path}\n")
    try:
        _dump(flow)
    except Exception:
        import traceback
        with open(OUT + ".err", "a", encoding="utf-8") as f:
            f.write(traceback.format_exc() + "\n")
