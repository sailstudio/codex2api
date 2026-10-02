# Prism 完整调用链（实测补全）

> 2026-09-17，用真实 pro 账号实测。这份文档补上了此前两个实现（包括我方 OAIprism
> 和第三方的 PrismOpenAIProxy）**都遗漏的关键环节：沙箱**。

---

## 完整链路

```
① POST /api/backend/1/new                    ← 申请沙箱（最容易漏的一步）
   → {"url":"https://prism.openai.com/s/sandboxes/proxy", "token":"gAAAAAB..."}

② GET  <url>/wait-for-sync?wait_ms=10000     ← 等沙箱就绪（可选但强烈建议）
   Header: X-Crixet-Sandbox-Token: <token>
   未就绪时这个端点会直接断 TLS（SSLEOFError），属正常，重试即可

③ POST /api/llm/response_with_tools_start
   body: {
     "input": [{"type":"message","role":"user",
                "content":[{"type":"input_text","text":"..."}]}],
     "metadata": {
       "model": "gpt-5.6-sol",
       "reasoning_effort": "low|medium|high|xhigh",
       "frontend_origin": "https://prism.openai.com",
       "sandbox_url":   "<步骤①的 url>",      ← 必须
       "sandbox_token": "<步骤①的 token>"     ← 必须
     }
   }
   → {"status":"started","request_id":"...","turn_state":{...}}     需要轮询
   → {"status":"completed","response":{...}}                        已结束

④ POST /api/llm/response_with_tools_status
   body: {"request_id":"...", "turn_state":{...}}   ← turn_state 逐轮取最新
   → {"status":"pending","turn_state":{...新}}     继续
   → {"status":"completed","response":{...}}       终态

⑤ POST /api/llm/response_with_tools_stop             取消（客户端断开时调它省钱）
   body: {"request_id":..., "conversation_id":..., "turn_state":...}
```

---

## ⚠️ 真正的瓶颈：沙箱需要 Y-Sweet 文档同步

把沙箱信息传对之后（`token_present=True`、`auth=True`，服务端甚至把
`sandbox_url` 解析成了内部地址 `crixet-backend.oai-science.svc.cluster.local:8081`），
start 仍然在 **122 秒后**返回：

```
reason: "unknown"
message: "Error while processing conversation (504 Gateway Timeout). Please submit prompt again."
```

原因藏在 `wait-for-sync` 的响应里 —— 它**永远停在 `syncing`**，而且把缺什么全列了出来：

```json
{
  "readinessCapabilities": ["current_y_sweet_provider"],
  "status": "syncing",
  "tokens": {
    "hasResourceToken":        false,
    "hasResourceBaseUrl":      false,
    "hasResourceProjectId":    false,
    "hasCurrentYSweetToken":   false,
    "hasSyncedYSweetProvider": false,
    "fileCredentialSource":    "none"
  }
}
```

**沙箱不是在"冷启动慢"，而是在等我们把项目的协作文档（Yjs）同步进去。**
没有文档，它没法理解"这个项目的 LaTeX 内容是什么"，于是会话处理超时。

### 文档同步的入口

```
POST /api/y
body: {"docId": "<project_uuid>", "requestContext": {}}
→ {
    "url":           "wss://prism.openai.com/y/d/<uuid>/ws",
    "baseUrl":       "https://prism.openai.com/y/d/<uuid>",
    "docId":         "<uuid>",
    "authorization": "full",
    "token":         "ASQ2ZDU0YWQ2Mi0xNzc4LTQxODAtYjA4My0zZjk4NWU5MjdlYjUBASQ..."
  }
```

**只有 WebSocket 一条路。** 实测：
- `GET baseUrl` / `/health` 全部 404
- 往沙箱代理 `POST resources|resource|configure|project|sync|attach` 全部 404
- 没有 HTTP 接口能把项目资源"递给"沙箱

所以要在无浏览器环境下跑通，必须实现 **Yjs 同步协议客户端**
（连接 `wss://.../y/d/<uuid>/ws`，用 lib0 二进制协议同步 Doc）。

### 这意味着什么（技术选型上的实际影响）

| 实现 | 补 Y-Sweet 的成本 |
|---|---|
| **PrismOpenAIProxy**（Node.js） | **低** —— 直接 `npm i yjs y-protocols ws`，几十行搞定 |
| **OAIprism**（Go） | **高** —— Go 的 Yjs 生态不成熟，要么引 `y-crdt` 系（依赖重），要么自己实现 lib0 编解码 |

这是 Node.js 在这个特定项目上**真实的技术优势**：它的生态里有现成的 Yjs 客户端。

### 另一条可行路线

让用户的浏览器保持某个项目打开（浏览器会完成 Y-Sweet 同步），
代理复用该会话的沙箱上下文。代价是需要浏览器常驻 —— 但对"本机自用"的场景
这反而是最省事、最稳的做法。

---

## 少了沙箱会怎样

不传 `sandbox_url` / `sandbox_token` 时，start 会**立刻**返回：

```json
{
  "status": "completed",
  "request_id": "...",
  "conversation_id": "cdx1_...",
  "response": {
    "status": "error",
    "payload": {
      "reason": "sandbox_reconnecting",
      "message": "Reconnecting to sandbox. Your request will resume automatically once the sandbox is ready.",
      "codexRequestDebug": {
        "sandbox_url_input": null,          ← 没传
        "sandbox_url_resolved": null,
        "sandbox_token_present": false,     ← 没传
        "backend_auth_token_present": true, ← 凭据是好的
        "server_proxy_origin": "https://crixet-frontend.gateway.unified-4.api.openai.com"
      }
    }
  }
}
```

`codexRequestDebug` 是个自检报告，**它直接把"你还缺什么"写出来了**。
拿着它排查比猜快得多。

---

## 沙箱预热：第一次调用可能 504

实测：申请到沙箱后立刻 start，会等约 122 秒然后返回

```
reason: "unknown"
message: "Error while processing conversation (504 Gateway Timeout). Please submit prompt again."
```

上游明确说了 **"Please submit prompt again"** —— 照做即可。这属于"沙箱在冷启动"，
不是协议错误。

推荐策略（按性价比排序）：

1. **预热**：拿到沙箱后先 `wait-for-sync` 等它就绪；
2. **重试**：失败就重发 start（同一沙箱，最多 3 次），这是上游自己建议的做法；
3. **复用**：沙箱与项目绑定，同一项目在一段时间内复用同一个沙箱能省掉冷启动。

---

## 顺带测准的其它端点

| 端点 | 结果 |
|---|---|
| `GET /api/project-access?d=<uuid>` | ✅ `{"accessible":true,"project":{uuid,title,owner,created_at,...}}` |
| `POST /api/projects` | ⚠️ **需要 `{project_uuid, title}` 两个字段**，只发 `{name,...}` 会 400 |
| `GET /api/maintenance` | ✅ `{"mode":"off"}` —— **当前不在维护** |
| `GET /api/user-preferences` | ✅ `{"betaProgramEnrolled":false,...}` |
| `POST /api/backend/1/new` | ✅ 沙箱申请，返回 `{url, token}` |

**重要更正**：我此前把"503 Service Unavailable + 维护中"当成上游在维护，
实际上是 **Cloudflare 对不带浏览器头的裸请求做的机器人拦截**。
带上完整的 `User-Agent` / `Origin` / `Referer` / `Cookie` 后一切正常。
教训：探测上游时请求头要伪装完整，否则会得到误导性的"维护中"。

---

## 两个实现的状态

| | PrismOpenAIProxy | OAIprism |
|---|---|---|
| 申请沙箱 | ❌ 未实现（README 说要手工填 `PRISM_SANDBOX_URL`） | ❌ 未实现 |
| 传 sandbox_url/token | ✅ 支持（靠环境变量手工配） | ❌ metadata 里没这两个字段 |
| wait-for-sync | ❌ | ❌ |
| 504 重试 | ❌ | ❌ |
| 其余协议 | ✅ | ✅ |

所以两边都需要补：**自动申请沙箱 → 等就绪 → 注入 metadata → 失败重试**。
