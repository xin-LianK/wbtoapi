#!/usr/bin/env python3
"""请求指标简报：昨日 status / outcome 分布，与前一日的环比对比。

数据源是网关自己的 JSONL 归档（internal/reqlog），按天一个文件：
    data/request-logs/requests-YYYY-MM-DD.jsonl

为什么要比环比而不是看绝对值：403 / 429 / 5xx 的绝对值取决于当天流量，
只有"占比"和"环比"才有意义。上游收紧校验时，最先动的就是这两个数，
比客户端协议变化还早（客户端那层是 `upstream-shape.py --watch` 的事）。

退出码：
    0  正常，或昨日无请求记录
    1  指标显著恶化（失败率环比 +2pp 以上，或 403/429 数量翻倍且绝对值 ≥3）
    2  参数/环境错误
"""

from __future__ import annotations

import argparse
import datetime as dt
import glob
import io
import json
import os
import sys

# 失败率上升超过这个百分点就报出来。2pp 是经验值：低于它多为流量结构波动，
# 高于它基本是上游行为变了。
FAIL_RATE_DELTA_PP = 2.0

# 403/429 翻倍判定要求的最小绝对值，避免 1 → 2 这种噪声被当成异常。
RATE_LIMIT_MIN_COUNT = 3

DEFAULT_DIR = os.path.join("data", "request-logs")


def parse_args():
    ap = argparse.ArgumentParser(
        description="请求指标简报（status / outcome 环比）",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    ap.add_argument("--dir", default=DEFAULT_DIR, help=f"JSONL 目录，默认 {DEFAULT_DIR}")
    ap.add_argument("--date", help="统计哪一天，YYYY-MM-DD，默认昨天")
    ap.add_argument("--json", action="store_true", help="输出 JSON（给自动化读）")
    ap.add_argument("--quiet", action="store_true", help="只在异常时输出")
    return ap.parse_args()


def day_path(directory: str, day: dt.date) -> str:
    """当天的主文件。日志轮转时会有 requests-YYYY-MM-DD-2.jsonl 这类分片。"""
    return os.path.join(directory, f"requests-{day:%Y-%m-%d}.jsonl")


def load_events(directory: str, day: dt.date):
    """读一天的所有分片。返回 (events, skipped, files)。

    分片按文件名排序，主文件排第一（没有后缀的在前）。
    """
    pattern = os.path.join(directory, f"requests-{day:%Y-%m-%d}*.jsonl")
    files = sorted(glob.glob(pattern))
    events, skipped = [], 0
    for path in files:
        try:
            with io.open(path, "r", encoding="utf-8", errors="replace") as f:
                for line in f:
                    line = line.strip()
                    if not line:
                        continue
                    try:
                        events.append(json.loads(line))
                    except json.JSONDecodeError:
                        skipped += 1
        except OSError:
            continue
    return events, skipped, files


def summarize(events):
    """把事件列表压成可比较的计数。"""
    total = len(events)
    ok = sum(1 for e in events if e.get("ok"))
    outcomes = {}
    statuses = {}
    retried = 0
    fail_paths = {}
    for e in events:
        oc = e.get("outcome") or "unknown"
        outcomes[oc] = outcomes.get(oc, 0) + 1
        # 只统计非 2xx，避免把 200 塞满分布表
        st = e.get("status") or 0
        if st < 200 or st >= 300:
            statuses[st] = statuses.get(st, 0) + 1
        if (e.get("attempts") or 0) > 1:
            retried += 1
        if not e.get("ok"):
            p = e.get("path") or "?"
            fail_paths[p] = fail_paths.get(p, 0) + 1
    failed = total - ok
    return {
        "total": total,
        "ok": ok,
        "failed": failed,
        "fail_rate": (failed / total * 100) if total else 0.0,
        "outcomes": outcomes,
        "statuses": statuses,
        "retried": retried,
        "fail_paths": dict(sorted(fail_paths.items(), key=lambda kv: -kv[1])[:5]),
    }


def rate_limit_count(s):
    """403 + 429：风控与限流的直接信号。"""
    return s["statuses"].get(403, 0) + s["statuses"].get(429, 0)


def fmt_delta(cur, prev, unit="", digits=1):
    """环比。prev 为空时返回空串——没有基线就不编造百分比。"""
    if prev is None:
        return ""
    d = cur - prev
    if abs(d) < 10 ** (-digits) / 2:
        return "（持平）"
    return f"（{'+' if d > 0 else ''}{d:.{digits}f}{unit}）"


def render(cur_day, cur, prev_day, prev, files, skipped):
    lines = []
    head = f"== 请求指标 {cur_day:%Y-%m-%d}"
    if prev is not None:
        head += f"（对比 {prev_day:%Y-%m-%d}）"
    lines.append(head)

    if cur["total"] == 0:
        lines.append("  昨日无请求记录。")
        if not files:
            lines.append(f"  未找到文件：{day_path(DEFAULT_DIR, cur_day)}")
            lines.append("  网关可能未运行，或日志归档在别处（--dir 指定）。")
        return lines

    lines.append(
        f"  总请求   {cur['total']:>6}  "
        f"{fmt_delta(cur['total'], prev['total'] if prev else None, ' 条', 0)}"
    )
    lines.append(
        f"  成功     {cur['ok']:>6}  {cur['ok'] / cur['total'] * 100:5.1f}%"
    )
    lines.append(
        f"  失败     {cur['failed']:>6}  {cur['fail_rate']:5.1f}%"
        + fmt_delta(cur["fail_rate"], prev["fail_rate"] if prev else None, "pp")
    )

    st = cur["statuses"]
    if st:
        parts = " ".join(f"{k}×{v}" for k, v in sorted(st.items()))
        lines.append(f"  非 2xx   {parts}")
    oc = {k: v for k, v in cur["outcomes"].items() if k != "success"}
    if oc:
        parts = " ".join(f"{k}×{v}" for k, v in sorted(oc.items(), key=lambda kv: -kv[1]))
        lines.append(f"  类型     {parts}")

    if cur["retried"]:
        pct = cur["retried"] / cur["total"] * 100
        lines.append(f"  重试     {cur['retried']:>6}  {pct:5.1f}% 的请求 attempts>1")
    if cur["fail_paths"]:
        parts = " ".join(f"{k}×{v}" for k, v in cur["fail_paths"].items())
        lines.append(f"  失败路径 {parts}")

    if skipped:
        lines.append(f"  跳过 {skipped} 行无法解析的记录")
    return lines


def judge(cur, prev):
    """返回 (是否恶化, 原因列表)。阈值见文件头。"""
    if cur["total"] == 0 or prev is None or prev["total"] == 0:
        return False, []
    reasons = []
    delta = cur["fail_rate"] - prev["fail_rate"]
    if delta >= FAIL_RATE_DELTA_PP:
        reasons.append(
            f"失败率 {prev['fail_rate']:.1f}% → {cur['fail_rate']:.1f}%（+{delta:.1f}pp）"
        )
    cr, pr = rate_limit_count(cur), rate_limit_count(prev)
    if cr >= RATE_LIMIT_MIN_COUNT and cr >= pr * 2 and cr > pr:
        reasons.append(f"403/429 从 {pr} 涨到 {cr}（风控或限流收紧）")
    return bool(reasons), reasons


def main():
    args = parse_args()
    today = dt.date.today()
    cur_day = (
        dt.date.fromisoformat(args.date) if args.date else today - dt.timedelta(days=1)
    )
    prev_day = cur_day - dt.timedelta(days=1)

    cur_events, skipped, files = load_events(args.dir, cur_day)
    prev_events, _, prev_files = load_events(args.dir, prev_day)
    cur = summarize(cur_events)
    prev = summarize(prev_events) if prev_files else None

    bad, reasons = judge(cur, prev)

    if args.json:
        print(
            json.dumps(
                {
                    "date": cur_day.isoformat(),
                    "prev_date": prev_day.isoformat() if prev else None,
                    "current": cur,
                    "previous": prev,
                    "files": [os.path.basename(f) for f in files],
                    "skipped_lines": skipped,
                    "degraded": bad,
                    "reasons": reasons,
                },
                ensure_ascii=False,
                indent=1,
            )
        )
        return 1 if bad else 0

    lines = render(cur_day, cur, prev_day, prev, files, skipped)
    if reasons:
        lines.append("  ⚠ " + "；".join(reasons))
    if not (args.quiet and not bad):
        print("\n".join(lines))
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
