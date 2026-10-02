# Prism 通道闭环 · 最终机理（全部真机实测钉死）

日期：2026-10-01 ｜ 账号：[REDACTED]（pro）｜ 出口：新加坡 [REDACTED]
底座：james-6-23/codex2api ｜ 新增包：`proxy/prism` ｜ 验证 CLI：`cmd/prism-live`

---

## 一、完整调用链

```
① 鉴权    Cookie: prism_oai_access_token=<access_token>; oai-sc=<同值>
          GET/POST /auth/session → 200，自动自举 prism_session_token
② sentinel header: openai-sentinel-token（严格一次性）
          由 node 就地跑 sentinel SDK 算 proof 铸造 → **无需浏览器**
③ 门禁    /api/*（projects / backend/1/new / llm/*）必须同时具备 ①+②
④ 对话    POST /api/llm/response_with_tools_start   {input, metadata, conversationId}
⑤ 轮询    POST /api/llm/response_with_tools_status  {request_id, turn_state}
          turn_state 必须逐轮原样回传，否则 400 "turn_state is required"
⑥ 取消    POST /api/llm/response_with_tools_stop    （省钱，停止未完成轮次）
```

## 二、五道门禁（每条都做过 A/B 实测）

| # | 门 | 要求 | 反例（实测结果） |
|---|---|---|---|
| 1 | sentinel | **每次请求现铸**（一次性） | 复用 → 403 `Request verification failed` |
| 2 | conversationId | **每次全新**（单向消费） | 复用 → 403 `Error while processing conversation` |
| 3 | metadata 全套 | **必须与材料同源**，含 `sandbox_url`/`sandbox_token`/`proxy_request_debug`/`codex_listen_snapshot` | 自铸新沙箱 → 400 `Please submit prompt again` |
| 4 | `metadata.model` + `reasoning_effort` | **也必须原样沿用材料里的值** | 改写 → 400（变体 C 实测） |
| 5 | 沙箱占用 | 材料产生后**必须中断侧车那条消息**（route.abort） | 放行 → 沙箱忙碌 → 403 |

## 三、响应解析（易错点）

上游终态响应是**两层嵌套**：

```json
{"status":"completed",
 "response":{"status":"success","payload":{
    "id":"resp_…",
    "output":[{"type":"message","role":"assistant",
               "content":[{"type":"output_text","text":"收到了"}]}],
    "usage":{…}, "message":"…"}}}
```

⚠️ 只读 `response.output` 会**永远取到空正文**，而且错误信息也被埋在这一层
（导致把 400/403 失败**误报成成功**）。必须剥到 `response.payload`。

## 四、metadata 必需字段（真机抓取）

```
projectId             项目 uuid
userId                ⚠️ [REDACTED-USER]（user- 前缀），不是 account_id UUID
model                 gpt-5.6-sol / gpt-6-astra / …（改写会 400）
reasoning_effort      low | medium | high（改写会 400）
frontend_origin       https://prism.openai.com
sandbox_url           https://prism.openai.com/s/sandboxes/proxy/（带尾斜杠）
sandbox_token         由 backend/1/new 产生
proxy_request_debug   ⭐ wait-for-sync 请求详情（材料自带，必须原样带）
codex_listen_snapshot ⭐ 会话/沙箱绑定快照（必须原样带）
```

## 五、材料（chat material）

- 来源：**真浏览器**打开项目页 → `page.route` 拦截 `start` 并 **abort** → 偷 body 写盘。
- 可复用性：**已验证**，但必须整包复用（含 `model`/`effort`/`sandbox_*`/快照）。
- 时效：`cf_bm`/`__cflb` 是分钟级 cookie → TTL 取 **2 分钟**。
- 侧车：`cmd/prism-material/produce.js`（playwright + 系统 Chrome + 代理 7890，headless 可用）。

## 六、闭环证据（2026-10-01）

```
prompt 「只回复三个字：收到了」   → 正文「收到了」   ✓
prompt 「计算 1+1，只回答数字」   → 正文「2」        ✓
```

Responses SSE 事件序列（下游消费形态，9 帧，顺序正确）：
```
response.created → response.in_progress
→ response.output_item.added → response.content_part.added
→ response.output_text.delta → response.output_text.done
→ response.content_part.done → response.output_item.done
→ response.completed
```

单测：`go test ./proxy/prism/` **10/10 PASS**
（payload 层级 / 扁平兼容 / 错误透出 / 工具信封解析 / cookie 拼装 / 工具协议 / 材料三种形态）

## 七、环境事实

- `curl` 会被 Cloudflare 拦（TLS 指纹）→ **必须 Go net/http 或真浏览器**。
- 页面内自动加载 `https://sentinel.openai.com/backend-api/sentinel/frame.html?sv=20260219f9f6`。
- sentinel SDK：`https://sentinel.openai.com/sentinel/20260219f9f6/sdk.js`（30KB，`node:vm` 可跑）。
- 无头 Chrome（playwright `channel=chrome`）可正常加载 Prism 并真实对话。
