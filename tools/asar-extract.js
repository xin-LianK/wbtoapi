// 解 asar：只导出 js/json 文本，跳过体积巨大的二进制与媒体资源。
const fs = require('fs');
const path = require('path');

const src = process.argv[2];
const outDir = process.argv[3];
const MAX = 20 * 1024 * 1024;

const fd = fs.openSync(src, 'r');
const head = Buffer.alloc(20);
fs.readSync(fd, head, 0, 20, 0);
// Chromium Pickle: [0]=4 [4]=pickle 总长 [8]=含 padding 的字符串长度 [12]=真实 JSON 长度
const pickleSize = head.readUInt32LE(4);
const jsonLen = head.readUInt32LE(12);
const jsonBuf = Buffer.alloc(jsonLen);
fs.readSync(fd, jsonBuf, 0, jsonLen, 16);
const header = JSON.parse(jsonBuf.toString('utf8'));
const dataOffset = 8 + pickleSize;

let count = 0, bytes = 0;
const keep = /\.(js|mjs|cjs|json|ts)$/i;

function walk(node, rel) {
  for (const [name, entry] of Object.entries(node.files || {})) {
    const p = path.join(rel, name);
    if (entry.files) { walk(entry, p); continue; }
    if (entry.unpacked || !entry.size || entry.size > MAX) continue;
    if (!keep.test(name)) continue;
    const buf = Buffer.alloc(entry.size);
    fs.readSync(fd, buf, 0, entry.size, dataOffset + Number(entry.offset));
    const dest = path.join(outDir, p);
    fs.mkdirSync(path.dirname(dest), { recursive: true });
    fs.writeFileSync(dest, buf);
    count++; bytes += entry.size;
  }
}
walk(header, '');
console.log(`extracted ${count} files, ${(bytes / 1048576).toFixed(1)} MB`);
