#!/usr/bin/env python3
"""上游形态快照：静态提取（解包）与抓包导入，输出同一份 schema，支持跨版本 diff。

设计依据见 MAINTENANCE.md §5。核心判断是**解包给全集、抓包给实例**：
  - 静态提取能看到条件分支与默认值（`e.enterpriseId?.trim()&&(...)`、
    `YN` 常量兜底表），给出所有可能发出的头；
  - 抓包只有实际走到的分支和真实取值，但能给出**请求体键集合**与真实头名大小写。

两者写进同一个 shape 结构，因此 diff 逻辑只有一份。

用法:
  # 对比：当前客户端 vs 仓库里最新的静态快照（有漂移退出码 1）
  python tools/upstream-shape.py

  # 存档：把当前客户端形态写入 snapshots/upstream-shape/<version>.json
  python tools/upstream-shape.py --save

  # 顺带核对本网关实际出站的头（值形态不一致也算漂移）
  python tools/upstream-shape.py --ours

  # 从抓包导入一份 capture 形态（HAR；Sunny/mitmproxy 的 HAR 导出均可）
  python tools/upstream-shape.py --har capture.har --save

  # 指定来源：app.asar 直接读（不解整包）/ 已解包目录
  python tools/upstream-shape.py --asar "C:/Program Files/WorkBuddy/resources/app.asar"
  python tools/upstream-shape.py --src ../_wb_client

退出码: 0 无漂移 / 1 有漂移 / 2 执行失败（如客户端结构变了）
"""

import argparse
import glob
import hashlib
import io
import json
import os
import re
import struct
import subprocess
import sys

SCHEMA = 1
SNAPSHOT_DIR = os.path.join("snapshots", "upstream-shape")

DEFAULT_ASAR_CANDIDATES = [
    r"C:\Program Files\WorkBuddy\resources\app.asar",
    r"C:\Program Files (x86)\WorkBuddy\resources\app.asar",
    os.path.expanduser("~/AppData/Local/Programs/WorkBuddy/resources/app.asar"),
]

# 静态提取的锚点：这几个函数名一变，说明客户端重构了，脚本必须报错而不是静默出空结果。
ANCHORS = [
    ("buildVlmGatewayHeaders", "function buildVlmGatewayHeaders"),
    ("applyGatewayHeaders", "function applyGatewayHeaders"),
    ("applyTraceHeaders", "function applyTraceHeaders"),
    ("YN", "var YN="),
]

# 不在上面三个函数里、但最终一定发出去的头（VlmUpstream.send 在 fetch 前补）。
# 入站头还会被透传一层（qN 黑名单剔除 origin/authorization/accept-encoding 等），
# 那部分取决于调用方，不作为客户端形态的一部分。
SUPPLEMENT = {
    "Accept-Encoding": {"from": "VlmUpstream.send", "when": "always",
                        "value": {"kind": "const", "detail": "identity"}},
    "Authorization": {"from": "VlmUpstream.send", "when": "always",
                      "value": {"kind": "template", "detail": "Bearer <accessToken>"}},
}


# ---------------------------------------------------------------------------
# app.asar 读取（Chromium Pickle：只读需要的成员，不解 96MB 整包）
# ---------------------------------------------------------------------------
def read_asar_members(path, wanted):
    """从 asar 里只抽出 wanted 里的成员，返回 {成员名: 文本}。

    布局：[0]=4 [4]=pickleSize [8]=含 padding 的串长 [12]=JSON 真实长度
          16 起是 JSON，数据区起点 = 8 + pickleSize。
    """
    out = {}
    fd = os.open(path, os.O_RDONLY | getattr(os, "O_BINARY", 0))
    try:
        head = os.read(fd, 20)
        if len(head) < 20:
            raise SystemExit(f"{path} 不是合法 asar（头部不足 20 字节）")
        pickle_size = struct.unpack("<I", head[4:8])[0]
        json_len = struct.unpack("<I", head[12:16])[0]
        # 注意：读完 20 字节后文件指针停在 20，而 JSON 从 16 开始，必须回退
        os.lseek(fd, 16, os.SEEK_SET)
        header = json.loads(os.read(fd, json_len).decode("utf-8"))
        data_off = 8 + pickle_size

        def walk(node, prefix):
            for name, entry in (node.get("files") or {}).items():
                rel = f"{prefix}{name}"
                if entry.get("files"):
                    walk(entry, rel + "/")
                    continue
                if rel in wanted and entry.get("size") and not entry.get("unpacked"):
                    os.lseek(fd, data_off + int(entry["offset"]), os.SEEK_SET)
                    out[rel] = os.read(fd, int(entry["size"])).decode("utf-8", "replace")

        walk(header, "")
        return out
    finally:
        os.close(fd)


def load_static_source(args):
    """返回 (version, server_js, meta)。支持 asar 直读与已解包目录两条路。"""
    if args.src:
        d = args.src
        js_path = os.path.join(d, "main", "server.js")
        if not os.path.exists(js_path):
            raise SystemExit(f"{d} 下没有 main/server.js，--src 要指向已解包目录")
        server_js = io.open(js_path, encoding="utf-8", errors="replace").read()
        version, meta = "", {}
        pkg = os.path.join(d, "package.json")
        if os.path.exists(pkg):
            try:
                pj = json.load(io.open(pkg, encoding="utf-8", errors="replace"))
                version = pj.get("version", "")
                meta["package_name"] = pj.get("name", "")
            except Exception:
                pass
        return version, server_js, meta

    asar = args.asar
    if not asar:
        for c in DEFAULT_ASAR_CANDIDATES:
            if os.path.exists(c):
                asar = c
                break
    if not asar or not os.path.exists(asar):
        raise SystemExit(
            "找不到 WorkBuddy 的 app.asar。用 --asar 指定，或用 --src 指向已解包目录。"
        )

    mem = read_asar_members(asar, {"main/server.js", "package.json"})
    if "main/server.js" not in mem:
        raise SystemExit(f"{asar} 里没有 main/server.js，客户端结构可能已变")

    version = ""
    try:
        version = json.loads(mem.get("package.json", "{}")).get("version", "")
    except Exception:
        pass

    meta = {
        "asar_path": asar,
        "asar_size": os.path.getsize(asar),
        "asar_mtime": int(os.path.getmtime(asar)),
    }
    # asar 体积大，整包哈希太慢；取头 1MB + 尾部 1MB 做指纹足够判定「有没有换过包」。
    h = hashlib.sha256()
    with open(asar, "rb") as f:
        h.update(f.read(1024 * 1024))
        f.seek(max(0, os.path.getsize(asar) - 1024 * 1024))
        h.update(f.read(1024 * 1024))
    meta["asar_fingerprint"] = h.hexdigest()[:16]
    return version, mem["main/server.js"], meta


# ---------------------------------------------------------------------------
# 表达式分类：把「值」压成可比较的形态，这是抓出「36 字符 vs 32 字符」的关键
# ---------------------------------------------------------------------------
def classify_expr(expr):
    """把 JS 表达式压成 {kind, detail}。kind 是跨版本可比的形态标签。"""
    e = (expr or "").strip()
    if not e:
        return {"kind": "unknown"}

    # 打包器会写成 `(0,W.randomBytes)(16).toString(`hex`)`，中间可能有 `)`，别写死
    m = re.search(r"randomBytes[^()]*\)?\((\d+)\)\.toString\([`'\"]?hex", e)
    if m:
        return {"kind": "hex", "detail": int(m.group(1)) * 2}
    if "randomUUID" in e:
        return {"kind": "uuid", "detail": 36}

    # 模板串里只剩常量 → const；含 ${} → template
    if e.startswith("`") and e.endswith("`") and "${" not in e:
        return {"kind": "const", "detail": e[1:-1]}
    if "${" in e:
        return {"kind": "template", "detail": e}

    # `x || `默认值`` / `x ?? `默认值`` 形态
    m = re.search(r"\|\|\s*`([^`]*)`\s*$", e) or re.search(r"\?\?\s*`([^`]*)`\s*$", e)
    if m:
        return {"kind": "input", "default": m.group(1)}
    if re.search(r"^e\.[A-Za-z]+(\?\.)?", e):
        return {"kind": "input"}
    return {"kind": "expr", "detail": e}


def classify_literal_value(val):
    """把**实际取值**压成形态标签，用于核对「我们发出去的值」和「官方声明」是否同形。"""
    v = (val or "").strip()
    if not v:
        return {"kind": "empty"}
    if re.fullmatch(r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}", v):
        return {"kind": "uuid", "detail": 36}
    if re.fullmatch(r"[0-9a-f]{32}", v):
        return {"kind": "hex", "detail": 32}
    if re.fullmatch(r"[0-9a-f]{16}", v):
        return {"kind": "hex", "detail": 16}
    if re.fullmatch(r"\d+\.\d+\.\d+([-+][\w.]+)?", v):
        return {"kind": "semver", "detail": v}
    if re.fullmatch(r"[0-9a-f]{32}-[0-9a-f]{16}-(0[01]|1[0-9a-f])", v):
        return {"kind": "w3c", "detail": "traceparent"}
    return {"kind": "const", "detail": v}


# ---------------------------------------------------------------------------
# 静态提取
# ---------------------------------------------------------------------------
def slice_between(src, start_marker, end_markers, limit=8000):
    i = src.find(start_marker)
    if i < 0:
        return None
    ends = [src.find(m, i + len(start_marker)) for m in end_markers]
    ends = [e for e in ends if e > 0]
    return src[i:min(ends)] if ends else src[i:i + limit]


def scan_value(seg, pos):
    """从 pos 开始读一个表达式，遇顶层 `,` / `)` / `}` / `;` 停止。"""
    depth, i, n = 0, pos, len(seg)
    while i < n:
        c = seg[i]
        if c in "([{":
            depth += 1
        elif c in ")]}":
            if depth == 0:
                break
            depth -= 1
        elif c == "," and depth == 0:
            break
        elif c == ";" and depth == 0:
            break
        i += 1
    return seg[pos:i]


def split_top_level(text, sep=","):
    """按顶层分隔符切分，忽略括号/花括号/字符串里的分隔符。"""
    parts, depth, cur, i, n = [], 0, [], 0, len(text)
    quote = None
    while i < n:
        c = text[i]
        if quote:
            cur.append(c)
            if c == quote and text[i - 1] != "\\":
                quote = None
            i += 1
            continue
        if c in "`'\"":
            quote = c
        elif c in "([{":
            depth += 1
        elif c in ")]}":
            depth -= 1
        elif c == sep and depth == 0:
            parts.append("".join(cur))
            cur = []
            i += 1
            continue
        cur.append(c)
        i += 1
    if cur:
        parts.append("".join(cur))
    return parts


def func_params(body):
    m = re.search(r"function\s+\w*\s*\(([^)]*)\)", body)
    if not m:
        return set()
    return {p.strip() for p in m.group(1).split(",") if p.strip()}


def local_aliases(body):
    """解析函数开头的 `let a=...,b=...;` 声明，得到 局部名 → 表达式。

    不解析这一步就会把 `X-IDE-Version: r` 记成 opaque，而 `r` 其实是
    `e.appVersion?.trim()||`0.0.0``——上一轮「X-IDE-Version 就是 0.0.0」
    的判断错误正是漏看了这层别名。
    """
    m = re.search(r"(?:let|var|const)\s+", body)
    if not m:
        return {}
    seg = scan_value(body, m.end())
    # scan_value 在顶层 `,` 停下，这里需要整条声明：从 m.end() 到下一个顶层 `;`
    start = m.end()
    depth, i = 0, start
    while i < len(body):
        c = body[i]
        if c in "([{":
            depth += 1
        elif c in ")]}":
            if depth == 0:
                break
            depth -= 1
        elif c == ";" and depth == 0:
            break
        i += 1
    decl = body[start:i]
    out = {}
    for chunk in split_top_level(decl):
        if "=" not in chunk:
            continue
        name, expr = chunk.split("=", 1)
        name = name.strip()
        if re.fullmatch(r"[A-Za-z_$][\w$]*", name):
            out[name] = expr.strip()
    return out


def resolve_alias(expr, aliases, params, depth=0):
    """把裸标识符替换成它的初始化表达式（最多 3 层），参数名保留不动。"""
    e = (expr or "").strip().rstrip(",")
    if depth > 3:
        return e
    if re.fullmatch(r"[A-Za-z_$][\w$]*", e):
        if e in aliases:
            return resolve_alias(aliases[e], aliases, params, depth + 1)
        return e
    # `r.acpConnectionId` 这类成员访问：解析前缀
    m = re.fullmatch(r"([A-Za-z_$][\w$]*)\.(\w+)", e)
    if m and m.group(1) in aliases and m.group(1) not in params:
        return f"{resolve_alias(aliases[m.group(1)], aliases, params, depth + 1)}.{m.group(2)}"
    return e


def enclosing_condition(body, pos):
    """判断某个赋值是否在 `cond && ( ... )` 组里，返回 `if xxx` 或 `always`。

    划重点：一个组里可能有多个赋值（企业号的 X-Enterprise-Id 与 X-Tenant-Id 共用一个
    条件），所以必须按括号回溯找组起点，不能只看紧跟的逗号。
    """
    depth, i = 0, pos - 1
    while i >= 0:
        c = body[i]
        if c == ")":
            depth += 1
        elif c == "(":
            if depth == 0:
                break
            depth -= 1
        i -= 1
    if i < 0:
        return "always"
    before = body[max(0, i - 60):i]
    m = re.search(r"e\.(\w+)\?\.trim\(\)&&\s*$", before)
    if m:
        return f"if {m.group(1)}"
    # 其它条件形态（如 `x > 0 && (`）保留原文尾巴，便于人工判断
    m = re.search(r"([\w.$?\[\]]+\s*(?:&&|\|\|)\s*)$", before)
    return "conditional" if m else "always"


def classify_expr_ctx(expr, params):
    """带参数上下文的分类：裸标识符若是函数形参，标成 param 而不是 unknown expr。"""
    e = (expr or "").strip()
    if re.fullmatch(r"[A-Za-z_$][\w$]*", e) and e in params:
        return {"kind": "param", "detail": e}
    m = re.fullmatch(r"([A-Za-z_$][\w$]*)\.(\w+)", e)
    if m and m.group(1) in params:
        return {"kind": "param", "detail": e}
    return classify_expr(e)


def parse_build_headers(body):
    out = {}
    aliases = local_aliases(body)
    params = func_params(body)
    for m in re.finditer(r"n\[`([^`]+)`\]=", body):
        name = m.group(1)
        eq = body.find("=", m.end() - 1)
        expr = resolve_alias(scan_value(body, eq + 1), aliases, params)
        out[name] = {
            "from": "buildVlmGatewayHeaders",
            "when": enclosing_condition(body, m.start()),
            "value": classify_expr_ctx(expr, params),
        }
    return out


def parse_set_calls(body, fn):
    out = {}
    aliases = local_aliases(body)
    params = func_params(body)
    for m in re.finditer(r"\.set\(`([^`]+)`,", body):
        name = m.group(1)
        expr = resolve_alias(scan_value(body, m.end()), aliases, params)
        out[name] = {"from": fn, "when": "always", "value": classify_expr_ctx(expr, params)}
    return out


def parse_const_table(src):
    """`var YN=[[`Name`,`value`],...]`：常量兜底表，只在头未设置时生效。"""
    i = src.find("var YN=")
    if i < 0:
        return {}
    seg = src[i:i + 800]
    out = {}
    for m in re.finditer(r"\[\s*`([^`]+)`\s*,\s*`([^`]*)`\s*\]", seg):
        out[m.group(1)] = {"from": "YN(default)", "when": "if unset", "value": {"kind": "const", "detail": m.group(2)}}
    return out


def extract_endpoints(src):
    eps = set()
    for m in re.finditer(r"[`'\"](/(?:v\d+|api|plugin|openapi)/[A-Za-z0-9_\-/{}.:]*)['\"`]", src):
        eps.add(m.group(1))
    return sorted(eps)


def extract_static(version, server_js, meta):
    structure, missing = {}, []
    for label, marker in ANCHORS:
        structure[label] = marker in server_js
        if marker not in server_js:
            missing.append(label)
    if missing:
        raise SystemExit(
            "客户端结构已变，以下锚点找不到：" + ", ".join(missing) +
            "\n→ 说明官方重构了这几个函数，先改本脚本的 ANCHORS 再谈对比。"
        )

    build_body = slice_between(server_js, "function buildVlmGatewayHeaders", ["function applyGatewayHeaders"])
    gw_body = slice_between(server_js, "function applyGatewayHeaders",
                            ["function applyTraceHeaders", "var YN="])
    tr_body = slice_between(server_js, "function applyTraceHeaders", ["function requireAccessToken"])

    headers = {}
    # 顺序有意义：后面的覆盖前面的（YN 是「缺则填」，优先级最低，最后合并但只补缺）
    for part in (parse_build_headers(build_body), parse_set_calls(gw_body, "applyGatewayHeaders"),
                 parse_set_calls(tr_body, "applyTraceHeaders")):
        headers.update(part)
    for name, item in parse_const_table(server_js).items():
        headers.setdefault(name, item)
    # 这三个头不在上面三个函数里，由 VlmUpstream.send 在 fetch 前补：
    #   for (k,v of req.headers) qN.has(k)||r.set(...)   // 入站透传，qN 黑名单剔掉
    #   applyGatewayHeaders(...)
    #   r.set(`Accept-Encoding`,`identity`); r.set(`Authorization`,`Bearer ${token}`)
    # 漏了这层会数错头（24 + 2 + X-User-Id = 27）。
    for name, item in SUPPLEMENT.items():
        headers.setdefault(name, item)

    return {
        "schema": SCHEMA,
        "source": "static",
        "client_version": version,
        "meta": meta,
        "structure": structure,
        "endpoints": extract_endpoints(server_js),
        "headers": dict(sorted(headers.items(), key=lambda kv: kv[0].lower())),
    }


# ---------------------------------------------------------------------------
# 抓包导入（HAR）
# ---------------------------------------------------------------------------
def ingest_har(path, host_filter=None):
    """把 HAR 里的请求压成形态：端点 + 头名→值形态 + 请求体键集合。"""
    doc = json.load(io.open(path, encoding="utf-8", errors="replace"))
    reqs = []
    for entry in (doc.get("log") or {}).get("entries") or []:
        req = entry.get("request") or {}
        url = req.get("url", "")
        m = re.match(r"https?://([^/]+)(/[^?#]*)", url)
        if not m:
            continue
        host, path_ = m.group(1), m.group(2)
        if host_filter and host_filter not in host:
            continue
        headers = {}
        for h in req.get("headers") or []:
            name = (h.get("name") or "").strip()
            if not name or name.startswith(":"):   # HTTP/2 伪头
                continue
            headers[name.lower()] = classify_literal_value(h.get("value"))
        body = {}
        text = ((req.get("postData") or {}).get("text") or "").strip()
        if text.startswith("{"):
            try:
                obj = json.loads(text)
                body["top"] = sorted(obj.keys())
                msgs = obj.get("messages") or []
                keys = set()
                for msg in msgs if isinstance(msgs, list) else []:
                    if isinstance(msg, dict):
                        keys |= set(msg.keys())
                if keys:
                    body["message"] = sorted(keys)
            except Exception:
                pass
        reqs.append({
            "host": host,
            "method": req.get("method", ""),
            "path": path_,
            "headers": dict(sorted(headers.items())),
            "body_keys": body,
        })
    # 同一 path 去重，取最完整的那条（头最多）
    best = {}
    for r in reqs:
        k = (r["method"], r["path"])
        if k not in best or len(r["headers"]) > len(best[k]["headers"]):
            best[k] = r
    return {
        "schema": SCHEMA,
        "source": "capture",
        "origin": os.path.basename(path),
        "requests": [best[k] for k in sorted(best)],
    }


# ---------------------------------------------------------------------------
# diff
# ---------------------------------------------------------------------------
FORM_EQUIV = {"uuid": {"uuid"}, "hex": {"hex"}}


def form_of(value):
    """官方声明形态 → 可比较的分类（hex 的位数单独比）。"""
    k = (value or {}).get("kind")
    if k in ("uuid", "hex", "const", "w3c"):
        return k
    return "opaque"


def diff_static(old, new):
    changes = []
    oh, nh = old.get("headers") or {}, new.get("headers") or {}
    for name in sorted(set(nh) - set(oh), key=str.lower):
        changes.append(("头新增", name, "—", describe(nh[name])))
    for name in sorted(set(oh) - set(nh), key=str.lower):
        changes.append(("头删除", name, describe(oh[name]), "—"))
    for name in sorted(set(oh) & set(nh), key=str.lower):
        a, b = oh[name], nh[name]
        fa, fb = form_of(a.get("value")), form_of(b.get("value"))
        if fa != fb:
            changes.append(("值形态变", name, describe(a), describe(b)))
        elif fa == "hex" and a["value"].get("detail") != b["value"].get("detail"):
            changes.append(("值长度变", name, describe(a), describe(b)))
        elif fa == "const" and a["value"].get("detail") != b["value"].get("detail"):
            changes.append(("常量变", name, describe(a), describe(b)))
        elif a.get("when") != b.get("when"):
            changes.append(("条件变", name, a.get("when"), b.get("when")))
        elif a.get("from") != b.get("from"):
            changes.append(("产出函数变", name, a.get("from"), b.get("from")))

    for ep in sorted(set(new.get("endpoints") or []) - set(old.get("endpoints") or [])):
        changes.append(("端点新增", ep, "—", "在客户端代码里出现"))
    for ep in sorted(set(old.get("endpoints") or []) - set(new.get("endpoints") or [])):
        changes.append(("端点消失", ep, "在客户端代码里出现", "—"))

    for label, ok in (new.get("structure") or {}).items():
        if not ok:
            changes.append(("结构缺失", label, "存在", "找不到"))
    return changes


def describe(item):
    if item is None:
        return "—"
    v = item.get("value") or {}
    s = form_of(v)
    if s == "hex":
        s += str(v.get("detail", "?"))
    elif s == "const":
        s += f"={v.get('detail')}"
    elif v.get("kind") == "input" and v.get("default"):
        s = f"input(默认 {v['default']})"
    elif v.get("kind") == "template":
        s = "template"
    return f"{s} [{item.get('from')}]"


def diff_capture(old, new):
    changes = []
    old_r = {(r["method"], r["path"]): r for r in old.get("requests") or []}
    new_r = {(r["method"], r["path"]): r for r in new.get("requests") or []}
    for k in sorted(set(new_r) - set(old_r)):
        changes.append(("请求新增", f"{k[0]} {k[1]}", "—", "抓到了新端点"))
    for k in sorted(set(old_r) - set(new_r)):
        changes.append(("请求消失", f"{k[0]} {k[1]}", "抓到过", "本次没抓到（可能只是没走到）"))
    for k in sorted(set(old_r) & set(new_r)):
        ho, hn = old_r[k]["headers"], new_r[k]["headers"]
        for name in sorted(set(hn) - set(ho)):
            changes.append(("头新增", f"{k[1]} {name}", "—", hn[name]["kind"]))
        for name in sorted(set(ho) - set(hn)):
            changes.append(("头消失", f"{k[1]} {name}", ho[name]["kind"], "—"))
        bo = old_r[k].get("body_keys") or {}
        bn = new_r[k].get("body_keys") or {}
        for layer in ("top", "message"):
            for key in sorted(set(bn.get(layer) or []) - set(bo.get(layer) or [])):
                changes.append(("body 键新增", f"{k[1]} body.{layer}.{key}", "—", "出现"))
            for key in sorted(set(bo.get(layer) or []) - set(bn.get(layer) or [])):
                changes.append(("body 键消失", f"{k[1]} body.{layer}.{key}", "出现", "—"))
    return changes


# ---------------------------------------------------------------------------
# 与「我们实际发出去的头」对账
# ---------------------------------------------------------------------------
def run_headers_dump():
    try:
        out = subprocess.check_output(["go", "run", "./cmd/headers-dump"], text=True)
    except Exception as e:
        return None, f"headers-dump 执行失败：{e}"
    got = {}
    for line in out.splitlines():
        m = re.match(r"^([A-Za-z0-9-]+):\s+(.+)$", line)
        if m:
            got[m.group(1)] = m.group(2).strip()
    return got, None


def diff_ours(official, ours):
    """官方声明形态 vs 我们实际发出的值形态。"""
    changes = []
    lower = {k.lower(): (k, v) for k, v in ours.items()}
    official_lower = {k.lower(): (k, v) for k, v in (official.get("headers") or {}).items()}

    for low, (name, item) in sorted(official_lower.items()):
        if low == "user-agent":
            if "user-agent" in lower:
                ua = lower["user-agent"][1]
                ver = official.get("client_version", "")
                if ver and f"WorkBuddy/{ver}" not in ua:
                    changes.append(("UA 版本段不符", "User-Agent", f"含 WorkBuddy/{ver}", ua))
            continue
        if item.get("when", "always") != "always":
            continue  # 条件头（企业/domain）取决于账号形态，不参与比对
        if low not in lower:
            changes.append(("少发", name, describe(item), "—"))
            continue
        actual = lower[low][1]
        want = item.get("value") or {}
        wk, got = form_of(want), classify_literal_value(actual)
        if wk == "opaque":
            continue
        if got["kind"] != wk:
            changes.append(("值形态不符", name, describe(item), f"{got['kind']}:{actual[:40]}"))
        elif wk == "hex" and want.get("detail") and got.get("detail") != want["detail"]:
            changes.append(("值长度不符", name, describe(item), f"{got['kind']}{got.get('detail')}"))
        elif wk == "const" and want.get("detail") != actual:
            changes.append(("常量不符", name, describe(item), actual))

    conditional = set()
    for low, (name, item) in official_lower.items():
        if item.get("when", "always") != "always":
            conditional.add(low)
    for low, (name, val) in sorted(lower.items()):
        if low not in official_lower:
            changes.append(("多发", name, "—", val))
    return changes, sorted(conditional)


# ---------------------------------------------------------------------------
# 快照读写
# ---------------------------------------------------------------------------
def snapshot_paths():
    if not os.path.isdir(SNAPSHOT_DIR):
        return []
    return sorted(glob.glob(os.path.join(SNAPSHOT_DIR, "*.json")))


def latest_static():
    """取版本号最高的 static 快照。版本号按数字段比较，不按字符串。"""
    best, best_key = None, None
    for p in snapshot_paths():
        try:
            d = json.load(io.open(p, encoding="utf-8"))
        except Exception:
            continue
        if d.get("source") != "static":
            continue
        key = tuple(int(x) for x in re.findall(r"\d+", str(d.get("client_version", "")))[:3])
        if best_key is None or key > best_key:
            best, best_key = d, key
    return best


def latest_capture():
    caps = []
    for p in snapshot_paths():
        try:
            d = json.load(io.open(p, encoding="utf-8"))
        except Exception:
            continue
        if d.get("source") == "capture":
            caps.append(d)
    return caps[-1] if caps else None


def save_path_of(shape):
    return os.path.join(SNAPSHOT_DIR, f"static-{shape.get('client_version') or 'unknown'}.json")


def save(shape, tag=""):
    os.makedirs(SNAPSHOT_DIR, exist_ok=True)
    if shape["source"] == "static":
        name = os.path.basename(save_path_of(shape))
    else:
        name = f"capture-{tag or shape.get('origin', 'har')}.json"
        name = re.sub(r"[^A-Za-z0-9_.-]", "-", name)
    path = os.path.join(SNAPSHOT_DIR, name)
    with io.open(path, "w", encoding="utf-8", newline="\n") as f:
        json.dump(shape, f, ensure_ascii=False, indent=2, sort_keys=False)
        f.write("\n")
    return path


# ---------------------------------------------------------------------------
def report(changes, title, detail_ok):
    if not changes:
        print(f"✅ {title}：无漂移")
        print(f"   {detail_ok}")
        return 0
    print(f"❌ {title}：{len(changes)} 处漂移")
    w1 = max(len(c[0]) for c in changes)
    w2 = max(len(str(c[1])) for c in changes)
    for kind, name, a, b in changes:
        print(f"   {kind:<{w1}}  {str(name):<{w2}}  {a}  →  {b}")
    return 1


def selftest(shape):
    """自检：手工制造 7 种漂移，确认每一种都能被抓到。

    一个抓不出漂移的检测器比没有更糟——它会给你「一致」的假结论。
    """
    import copy
    cases = []

    def add(label, mutate, want_kinds, base=None):
        s = copy.deepcopy(base or shape)
        mutate(s)
        got = {c[0] for c in diff_static(shape, s)}
        ok = want_kinds <= got
        cases.append((ok, label, sorted(got) or ["(无)"]))
        return s

    names = sorted(shape["headers"], key=str.lower)
    add("删掉一个头", lambda s: s["headers"].pop(names[0]), {"头删除"})
    add("新增一个头", lambda s: s["headers"].__setitem__(
        "X-New-Header", {"from": "test", "when": "always",
                         "value": {"kind": "const", "detail": "1"}}), {"头新增"})

    def to_hex(s):
        s["headers"][names[-1]] = {"from": "test", "when": "always",
                                   "value": {"kind": "hex", "detail": 32}}
    add("值形态 uuid→hex", to_hex, {"值形态变"})

    hexy = next(k for k, v in shape["headers"].items() if v["value"].get("kind") == "hex")

    def double_len(s):
        # 不能写死 16：可能挑到 hex16 的头（X-B3-ParentSpanId），改完没变化就测不出东西
        s["headers"][hexy]["value"]["detail"] = s["headers"][hexy]["value"]["detail"] * 2

    add("值长度翻倍", double_len, {"值长度变"})

    consty = next((k for k, v in shape["headers"].items()
                   if v["value"].get("kind") == "const" and v["when"] == "always"), None)
    if consty:
        add("常量改动", lambda s: s["headers"][consty]["value"].__setitem__("detail", "CHANGED"), {"常量变"})

    condy = next((k for k, v in shape["headers"].items() if v["when"] != "always"), None)
    if condy:
        add("条件头变必发", lambda s: s["headers"][condy].__setitem__("when", "always"), {"条件变"})

    add("端点新增", lambda s: s["endpoints"].append("/v2/chat/completions-v3"), {"端点新增"})

    # 我们的出站头对账也要能报错：把 32 位 hex 改成 16 位必须被抓
    ours_ok, _ = run_headers_dump()
    if ours_ok:
        fake = dict(ours_ok)
        for k in fake:
            if re.fullmatch(r"[0-9a-f]{32}", fake[k]):
                fake[k] = fake[k][:16]
                break
        ch, _ = diff_ours(shape, fake)
        cases.append((any(c[0] == "值长度不符" for c in ch),
                      "出站头值长度不符", sorted({c[0] for c in ch}) or ["(无)"]))
        ch2, _ = diff_ours(shape, {k: v for k, v in ours_ok.items() if k.lower() != "x-trace-id"})
        cases.append((any(c[0] == "少发" for c in ch2), "出站头漏发",
                      sorted({c[0] for c in ch2}) or ["(无)"]))

    print("自检：")
    bad = 0
    for ok, label, got in cases:
        print(f"  {'✅' if ok else '❌'} {label:<18} 抓到 {got}")
        bad += 0 if ok else 1
    print(f"{len(cases)} 项，{len(cases) - bad} 通过" + ("" if not bad else f"，{bad} 失败"))
    return 0 if not bad else 2


def main():
    ap = argparse.ArgumentParser(add_help=True, description="上游形态快照与漂移检测")
    ap.add_argument("--save", action="store_true", help="把本次结果存档到 snapshots/upstream-shape/")
    ap.add_argument("--ours", action="store_true", help="顺带核对本网关的实际出站头")
    ap.add_argument("--har", metavar="PATH", help="从 HAR 抓包导入一份 capture 形态")
    ap.add_argument("--host", default="copilot.tencent.com", help="HAR 过滤的主机名（默认 copilot.tencent.com）")
    ap.add_argument("--asar", metavar="PATH", help="app.asar 路径（默认自动查找）")
    ap.add_argument("--src", metavar="DIR", help="已解包目录（含 main/server.js），跳过解包")
    ap.add_argument("--baseline", metavar="PATH", help="指定对比基线 JSON（默认取最新快照）")
    ap.add_argument("--list", action="store_true", help="列出已有快照")
    ap.add_argument("--selftest", action="store_true", help="自检：制造 7 类漂移确认都能抓到")
    ap.add_argument("--watch", action="store_true",
                    help="每日入口：版本没变就一行退出（0），变了才跑完整对比")
    args = ap.parse_args()

    if args.list:
        ps = snapshot_paths()
        if not ps:
            print("还没有快照。先跑 --save 建基线。")
        for p in ps:
            d = json.load(io.open(p, encoding="utf-8"))
            extra = d.get("client_version") or d.get("origin") or ""
            n = len(d.get("headers") or d.get("requests") or {})
            print(f"  {p}  {d.get('source')}  {extra}  条目 {n}")
        return 0

    # ---- 抓包导入模式 ----
    if args.har:
        shape = ingest_har(args.har, args.host)
        if not shape["requests"]:
            raise SystemExit(f"{args.har} 里没有匹配 {args.host} 的请求")
        base = latest_capture()
        if args.baseline:
            base = json.load(io.open(args.baseline, encoding="utf-8"))
        if args.save:
            print(f"已存档 {save(shape, tag=os.path.splitext(os.path.basename(args.har))[0])}")
        if not base:
            print(f"首次抓包形态：{len(shape['requests'])} 条请求，无基线可对比")
            return 0
        return report(diff_capture(base, shape),
                      f"抓包形态 vs {base.get('origin', '上一份')}",
                      f"{len(shape['requests'])} 条请求")

    # ---- 静态提取模式 ----
    version, server_js, meta = load_static_source(args)
    shape = extract_static(version, server_js, meta)

    if args.watch:
        base = latest_static()
        if base and base.get("client_version") == version:
            # 版本没变：同版本的静态提取结果必然逐字节相同，直接跳过全部重活。
            print(f"客户端 {version} 未变（基线 {os.path.basename(save_path_of(base))}），跳过")
            return 0
        print(f"客户端版本变化：{base.get('client_version') if base else '(无基线)'} → {version}")
        rc = report(diff_static(base, shape) if base else [],
                    f"静态形态 → {version}", f"{len(shape['headers'])} 个头")
        return rc

    if args.selftest:
        return selftest(shape)

    rc = 0
    if args.save:
        print(f"已存档 {save(shape)}")

    base = json.load(io.open(args.baseline, encoding="utf-8")) if args.baseline else latest_static()
    if base is None:
        print(f"客户端 {version or '未知版本'}：{len(shape['headers'])} 个头、"
              f"{len(shape['endpoints'])} 个端点。")
        print("还没有基线快照，本次只输出不对比。加 --save 建基线。")
    else:
        title = f"静态形态 {base.get('client_version')} → {version or '未知'}"
        rc |= report(diff_static(base, shape), title,
                     f"{len(shape['headers'])} 个头、{len(shape['endpoints'])} 个端点、"
                     f"结构锚点 {sum(1 for v in shape['structure'].values() if v)}/{len(shape['structure'])}")

    if args.ours:
        ours, err = run_headers_dump()
        if err:
            print(f"⚠️  {err}")
            rc |= 2
        else:
            ch, cond = diff_ours(shape, ours)
            rc |= report(ch, f"本网关出站头 vs 官方 {version or ''}",
                         f"已比 {len(ours)} 个头；条件头 {len(cond)} 个按账号形态跳过")
    return rc


if __name__ == "__main__":
    sys.exit(main())
