#!/bin/bash
# Prism 通道冒烟验证：纯文本 + 带图。
#
#   环境变量：
#     BASE  网关地址      默认 http://127.0.0.1:8081
#     KEY   prism 渠道 Key（必填）
#
# 用法：KEY=<prism渠道Key> bash deploy/prism/verify.sh
set -euo pipefail

: "${KEY:?必须提供 prism 渠道 Key}"
BASE="${BASE:-http://127.0.0.1:8081}"

echo "══ ① 纯文本（预期 10-30s）══"
t0=$(date +%s)
curl -sS -m 200 -X POST "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"只回复两个字：好的"}]}' \
  -o /tmp/prism_verify_text.json
echo "  HTTP 完成，耗时 $(( $(date +%s) - t0 ))s"
python3 - <<'PY'
import json
d = json.load(open('/tmp/prism_verify_text.json'))
print("  回答:", repr((d.get('choices') or [{}])[0].get('message', {}).get('content'))[:80])
PY

echo ""
echo "══ ② 带图（预期 30-90s）══"
# 造一张左上角有黑块的白图（不依赖 PIL：手写最小 PNG 不可行，改用 base64 常量为空则跳过）
python3 - "$BASE" "$KEY" <<'PY'
import base64, json, struct, subprocess, sys, zlib, io
BASE, KEY = sys.argv[1], sys.argv[2]

# 手写 512x512 PNG：左上 128x128 黑块
W = H = 512
rows = []
for y in range(H):
    row = bytearray([0])  # filter type
    for x in range(W):
        black = x < 128 and y < 128
        row += b'\x00\x00\x00' if black else b'\xff\xff\xff'
    rows.append(bytes(row))
raw = b''.join(rows)

def chunk(tag, data):
    return struct.pack('>I', len(data)) + tag + data + struct.pack('>I', zlib.crc32(tag + data) & 0xffffffff)

png = (b'\x89PNG\r\n\x1a\n'
       + chunk(b'IHDR', struct.pack('>IIBBBBB', W, H, 8, 2, 0, 0, 0))
       + chunk(b'IDAT', zlib.compress(raw, 9))
       + chunk(b'IEND', b''))
b64 = base64.b64encode(png).decode()
print(f"  图已生成：{len(png)} 字节 / base64 {len(b64)} 字符")

body = {"model": "gpt-5.6-sol", "messages": [{"role": "user", "content": [
    {"type": "text", "text": "白色画布上有一个黑色方块，它在画面的哪个角？只回答：左上、右上、左下、右下 之一。"},
    {"type": "image_url", "image_url": {"url": f"data:image/png;base64,{b64}"}}]}]}

import time
t0 = time.time()
r = subprocess.run(["curl", "-sS", "-m", "240", "-X", "POST", f"{BASE}/v1/chat/completions",
                    "-H", f"Authorization: Bearer {KEY}", "-H", "Content-Type: application/json",
                    "-d", json.dumps(body)], capture_output=True, text=True)
print(f"  HTTP 完成，耗时 {time.time() - t0:.1f}s")
try:
    d = json.loads(r.stdout)
    ans = (d.get('choices') or [{}])[0].get('message', {}).get('content')
    print("  回答:", repr(ans)[:120])
    print("  判定:", "✅ 正确（左上）" if ans and '左上' in ans else "❌ 期望「左上」")
except Exception:
    print("  响应:", r.stdout[:300] or r.stderr[:300])
PY

echo ""
echo "提示：模型是否真的看到图，不能只凭单次方位回答 —— 换角度复测（把黑块画到右下）后再下结论。"
