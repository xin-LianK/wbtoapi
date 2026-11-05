# SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
# SPDX-License-Identifier: AGPL-3.0-only
# 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

"""在压缩 JS 中定位动态拼接的请求头名。

用法: python find_hdr.py <文件> <关键字> [前后字符数]
"""
import io
import re
import sys

path = sys.argv[1]
key = sys.argv[2]
pad = int(sys.argv[3]) if len(sys.argv) > 3 else 400

d = io.open(path, encoding="utf-8", errors="replace").read()
print(f"size={len(d):,}")

for m in list(re.finditer(re.escape(key), d))[:5]:
    i = m.start()
    print(f"@{i} " + "-" * 60)
    print(d[max(0, i - pad):i + pad].replace("\n", " "))
    print()

# 定向找 Ot/Dt 这类短名常量的字符串定义
print("=" * 70)
print("短名常量定义（Ot/Dt/kt 等 = 字符串）")
for name in ["Ot", "Dt", "kt"]:
    for m in list(re.finditer(rf'\b{name}\s*=\s*["\'`]([^"\'`]{{2,60}})["\'`]', d))[:6]:
        print(f"  {name} = {m.group(1)!r}  @{m.start()}")
