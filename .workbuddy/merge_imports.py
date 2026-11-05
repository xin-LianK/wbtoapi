#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
# SPDX-License-Identifier: AGPL-3.0-only
# 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

"""合并 git 冲突文件中的纯 import 冲突块。

判据：冲突块两侧所有非空行都是 import 语句（可带别名），则视为机械冲突，
两侧并集去重。同时把 yreidev 的 module 路径改回本仓库的。
"""
import re
import subprocess
import sys

OURS_MOD = "github.com/linguo2625469/workbuddy2api-panel"
THEIRS_MOD = "github.com/yreidev/workbuddy2api-panel"

IMPORT_RE = re.compile(r'^\s*(?:[A-Za-z_][A-Za-z0-9_]*\s+)?"[^"]+"\s*$')


def is_import_only(lines):
    non_empty = [ln for ln in lines if ln.strip()]
    if not non_empty:
        return False
    return all(IMPORT_RE.match(ln) for ln in non_empty)


def merge_import_block(ours, theirs):
    seen, out = set(), []
    for ln in ours + theirs:
        key = ln.strip()
        if not key or key in seen:
            continue
        seen.add(key)
        out.append(ln.rstrip())
    return out


def process(path):
    with open(path, encoding="utf-8") as f:
        lines = f.read().split("\n")

    out, i, resolved, pending = [], 0, 0, 0
    while i < len(lines):
        if lines[i].startswith("<<<<<<<"):
            start = i
            ours, theirs = [], []
            i += 1
            while i < len(lines) and not lines[i].startswith("======="):
                ours.append(lines[i]); i += 1
            i += 1
            while i < len(lines) and not lines[i].startswith(">>>>>>>"):
                theirs.append(lines[i]); i += 1
            i += 1  # 跳过 >>>>>>>
            if is_import_only(ours) and is_import_only(theirs):
                out.extend(merge_import_block(ours, theirs))
                resolved += 1
            else:
                out.extend(lines[start:i])
                pending += 1
        else:
            out.append(lines[i]); i += 1

    text = "\n".join(out).replace(THEIRS_MOD, OURS_MOD)
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        f.write(text)
    return resolved, pending


def main():
    files = subprocess.run(
        ["git", "diff", "--name-only", "--diff-filter=U"],
        capture_output=True, text=True, check=True).stdout.split()
    total_r = total_p = 0
    for p in files:
        try:
            r, pend = process(p)
        except UnicodeDecodeError:
            print(f"  跳过（非文本）: {p}")
            continue
        total_r += r; total_p += pend
        if r:
            print(f"  {p}: 自动合并 {r} 个 import 块，待手工 {pend}")
    print(f"\n合计：自动解决 {total_r} 个 import 块，仍待手工 {total_p} 个语义块")


if __name__ == "__main__":
    sys.exit(main())
