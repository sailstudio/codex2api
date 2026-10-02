#!/bin/bash
# 下载 Prism 门禁所需的 sentinel SDK（本地 node 铸造用的运行时资产）。
#
# ⚠️ 为什么不入库、也不硬编码版本：
#   1) proxy/prism/assets/sdk.js 是上游 OpenAI 的「混淆专有资产」，无 license 头，
#      再分发有风险 —— 且它是**运行时资产**，不是本项目的源码。
#   2) 路径里的 SV 段（如 20260219f9f6）**会随上游漂移**。硬编码会在上游升级后静默失效，
#      表现为铸造失败、且很难排查。故本脚本从 stub **自动发现**当前 SV。
#   3) 仓库上游（james-6-23/codex2api）没有 vendor JS 资产的先例。
#
# 用法：
#   bash cmd/prism-sentinel/fetch-sdk.sh
#   PRISM_SENTINEL_SDK=/path/to/sdk.js bash cmd/prism-sentinel/fetch-sdk.sh
#
# 离线部署：在能出网的机器上跑本脚本，再把产物拷到目标机的 $PRISM_SENTINEL_SDK。
set -euo pipefail

ORIGIN="${PRISM_SENTINEL_ORIGIN:-https://sentinel.openai.com}"
OUT="${PRISM_SENTINEL_SDK:-/tmp/prism_sidecar/assets/sdk.js}"
UA="${PRISM_UA:-Mozilla/5.0}"

mkdir -p "$(dirname "$OUT")"

# 1) 从 stub 自动发现当前 SV（stub 内嵌 script.src=…/sentinel/<SV>/sdk.js）
STUB=$(curl -fsS -A "$UA" --max-time 25 "$ORIGIN/backend-api/sentinel/sdk.js")
SV=$(printf '%s' "$STUB" | grep -oE 'sentinel/[0-9a-f]+/sdk\.js' | head -1 | cut -d/ -f2 || true)
if [ -z "$SV" ]; then
  echo "❌ 无法从 stub 发现 SV，stub 内容片段：" >&2
  printf '%s\n' "$STUB" | head -c 200 >&2
  echo >&2
  exit 1
fi
echo "→ 发现 SV = $SV"

# 2) 下载 sdk.js
TMP="$OUT.tmp"
if ! curl -fsS -A "$UA" --max-time 40 -o "$TMP" "$ORIGIN/sentinel/$SV/sdk.js"; then
  echo "❌ 下载失败：$ORIGIN/sentinel/$SV/sdk.js" >&2
  rm -f "$TMP"
  exit 1
fi

# 3) 内容嗅探 —— **不绑定 sha256**：SDK 会随上游升级，绑死会导致正常升级被误判为损坏。
#    改为校验「确实是那份 JS」，这足够挡住 HTML 错误页 / 空响应 / 重定向页。
if head -c 300 "$TMP" | grep -qiE '<!doctype|<html'; then
  echo "❌ 拿到的是 HTML 页面而非 JS（可能被 WAF/登录页拦截）" >&2
  rm -f "$TMP"; exit 1
fi
if ! grep -q 'SentinelSDK' "$TMP"; then
  echo "❌ 下载内容中未找到 SentinelSDK 导出，疑似非目标资产" >&2
  rm -f "$TMP"; exit 1
fi
SIZE=$(wc -c < "$TMP" | tr -d ' ')
if [ "$SIZE" -lt 5000 ]; then
  echo "❌ 文件过小（${SIZE}B），疑似非真资产" >&2
  rm -f "$TMP"; exit 1
fi

mv "$TMP" "$OUT"
HASH=$(shasum -a 256 "$OUT" | awk '{print $1}')
echo "OK: wrote ${OUT} (${SIZE} bytes)"
echo "    sha256 = ${HASH}"
echo "    note: SV=${SV} drifts upstream; this script auto-discovers it - do not hardcode."
