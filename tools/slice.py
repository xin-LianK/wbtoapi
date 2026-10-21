import sys, re

path = sys.argv[1]
keys = sys.argv[2].split('|')
ctx = int(sys.argv[3]) if len(sys.argv) > 3 else 300
limit = int(sys.argv[4]) if len(sys.argv) > 4 else 6

with open(path, 'r', encoding='utf-8', errors='replace') as f:
    data = f.read()

print(f"file size = {len(data):,}")
for k in keys:
    idxs = [m.start() for m in re.finditer(re.escape(k), data)]
    print(f"\n{'='*70}\n[{k}] 命中 {len(idxs)} 次")
    for i in idxs[:limit]:
        seg = data[max(0, i - ctx): i + ctx].replace('\n', ' ')
        print(f"--- @{i} ---\n{seg}\n")
