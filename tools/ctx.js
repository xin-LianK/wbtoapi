// SPDX-FileCopyrightText: 2026 yreidev 与贡献者（见 NOTICE）
// SPDX-License-Identifier: AGPL-3.0-only
// 复制或改写本文件须保留以上声明，并按 NOTICE 的署名条款注明出处：https://github.com/linguo2625469/workbuddy2api-panel

// 在解包后的客户端代码里按关键词打印上下文（文件多为压缩单行，grep -C 无用）。
const fs = require('fs');
const [file, needle, before = '500', after = '700'] = process.argv.slice(2);
const s = fs.readFileSync(file, 'utf8');
let idx = 0, n = 0;
while ((idx = s.indexOf(needle, idx)) !== -1 && n < 6) {
  const a = Math.max(0, idx - Number(before));
  const b = Math.min(s.length, idx + needle.length + Number(after));
  console.log(`\n===== match ${++n} @ ${idx} =====\n` + s.slice(a, b));
  idx += needle.length;
}
if (!n) console.log('not found');
