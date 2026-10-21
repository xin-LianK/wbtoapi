"""从官方客户端分发代码里提取 chat 出站头集合，与本网关的实际出站头对账。

用法:
    python tools/header-parity.py <解包目录/main/server.js> [<dump.txt>]

不传 dump.txt 时，脚本自己调用 `go run ./cmd/headers-dump` 取当前形态。

依据（WorkBuddy 桌面端，app.asar → main/server.js）:
    buildVlmGatewayHeaders  → 身份头（X-IDE-* / X-Product / UA ...）
    applyGatewayHeaders     → 协议头（X-Conversation-* / trace / 常量批）
    applyTraceHeaders       → W3C traceparent + b3 族
    YN                      → 兜底常量表（applyGatewayHeaders 末尾按「缺则填」合并）
    末尾两行                 → Accept-Encoding / Authorization

退出码: 0 = 集合一致（差异只在允许的「条件头」），1 = 有实质差异。
"""
import io
import json
import os
import re
import subprocess
import sys

# 官方「有值才发」的条件头：本网关非企业号样例里不该出现，出现差异不算错。
CONDITIONAL = {"X-Enterprise-Id", "X-Tenant-Id", "X-Domain"}
# 官方把入站头排除后又手工设置的，最终一定有：出现与否都按必发算。
ALWAYS = {"Accept-Encoding", "Authorization"}

NAME_RE = re.compile(r"`([A-Za-z][A-Za-z0-9-]*[A-Za-z0-9])`")
# 不带连字符、只能靠白名单认出来的头名。
BARE_HEADERS = {"accept", "authorization", "traceparent", "b3"}


def slice_fn(src, start_marker, end_markers):
    """按函数名切出函数体：从 start_marker 到最近的 end_marker 中先出现者。"""
    i = src.find(start_marker)
    if i < 0:
        raise SystemExit(f"找不到 {start_marker}，客户端结构可能变了")
    ends = [src.find(m, i + len(start_marker)) for m in end_markers]
    ends = [e for e in ends if e > 0]
    return src[i:min(ends)] if ends else src[i:i + 4000]


def extract_official(src):
    """返回 {头名: 产出函数}。"""
    out = {}
    parts = [
        ("buildVlmGatewayHeaders",
         slice_fn(src, "function buildVlmGatewayHeaders", ["function applyGatewayHeaders"])),
        ("applyGatewayHeaders",
         slice_fn(src, "function applyGatewayHeaders", ["function applyTraceHeaders", "var YN="])),
        ("applyTraceHeaders",
         slice_fn(src, "function applyTraceHeaders", ["function requireAccessToken"])),
        ("YN",
         slice_fn(src, "var YN=", ["function applyTraceHeaders"])),
    ]
    for fn, body in parts:
        for m in NAME_RE.finditer(body):
            name = m.group(1)
            low = name.lower()
            # 只认两种：带连字符的（x-requested-with / acp-connection-id / X-IDE-Type），
            # 以及少量不带连字符的白名单名（accept / b3 / traceparent）。
            # 其余反引号字符串是取值（`Saas` / `craft` / `identity`），不是头名。
            if "-" not in name and low not in BARE_HEADERS:
                continue
            if low in BARE_HEADERS and low not in ("b3", "traceparent"):
                name = name
            out.setdefault(name, fn)
    for name in ALWAYS:
        out.setdefault(name, "send()")
    return out


def read_dump(path):
    text = io.open(path, encoding="utf-8", errors="replace").read() if path else ""
    if not text:
        text = subprocess.check_output(
            ["go", "run", "./cmd/headers-dump"], text=True
        )
    got = {}
    for line in text.splitlines():
        m = re.match(r"^([A-Za-z0-9-]+):\s+(.+)$", line)
        if m:
            got[m.group(1)] = m.group(2).strip()
    return got, text


def main():
    if len(sys.argv) < 2:
        raise SystemExit(__doc__)
    src = io.open(sys.argv[1], encoding="utf-8", errors="replace").read()
    official = extract_official(src)
    got, raw = read_dump(sys.argv[2] if len(sys.argv) > 2 else "")

    norm = lambda s: s.lower()
    off = {norm(k): k for k in official}
    ours = {norm(k): k for k in got}

    missing = [off[k] for k in sorted(off) if k not in ours and off[k] not in CONDITIONAL]
    extra = [ours[k] for k in sorted(ours) if k not in off]
    matched = [off[k] for k in sorted(off) if k in ours]
    cond_absent = [off[k] for k in sorted(off) if k not in ours and off[k] in CONDITIONAL]

    print(f"官方 chat 头 {len(official)} 个 / 本网关 {len(got)} 个")
    print(f"\n✅ 一致 {len(matched)}:")
    for k in matched:
        print(f"   {k}")
    if cond_absent:
        print(f"\n➖ 条件头（本形态不发，符合预期）{len(cond_absent)}: {', '.join(cond_absent)}")
    if missing:
        print(f"\n❌ 少发 {len(missing)}:")
        for k in missing:
            print(f"   {k}  (来源 {official[k]})")
    if extra:
        print(f"\n❌ 多发 {len(extra)}:")
        for k in extra:
            print(f"   {k}")
    if not missing and not extra:
        print("\n结论：头的集合一致（大小写不敏感）。")
        sys.exit(0)
    sys.exit(1)


if __name__ == "__main__":
    main()
