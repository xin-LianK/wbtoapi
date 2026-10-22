"""mitmproxy 插件：把命中上游主机的请求压成 JSONL（头 + body 前 16KB）。

配合 tools/capture-upstream.sh 使用。只记录业务主机，避免被遥测/本地回环淹没。
"""
import json
import os
import re
import time

from mitmproxy import http

OUT = os.environ.get("MITM_OUT", "capture/upstream.jsonl")
HOST_RE = os.environ.get("MITM_HOST_RE", r"tencent\.com|copilot|workbuddy|codebuddy")
BODY_LIMIT = int(os.environ.get("MITM_BODY_LIMIT", "16000"))

os.makedirs(os.path.dirname(OUT) or ".", exist_ok=True)
_host = re.compile(HOST_RE)


class UpstreamDump:
    def response(self, flow: http.HTTPFlow) -> None:
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
