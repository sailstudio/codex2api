# 02 · alanbulan/oai-prism 深度逆向研究

- 仓库：`alanbulan/oai-prism`（模块名 `github.com/oai-prism/oaiprism`）
- 本地路径：`/Users/chiptest/codex-prism/repos/oai-prism`
- HEAD：`f3d4c3cd3f2c3feb3885f55d3f983f22ae93f24b`（2026-10-01 21:17:42 +0800）
- 规模：Go，21,288 行（含测试），`go 1.26.0`
- 依赖：**直接依赖只有 1 个**（`gopkg.in/yaml.v3`）；SQLite 走 `modernc.org/sqlite`（纯 Go 无 CGO）+ `golang.org/x/time/rate`，其余全是间接依赖
- 定位：把 `prism.openai.com` 的内部 start/poll 协议包装成 OpenAI Chat Completions / Responses / Anthropic Messages，同时保留一条 `/prism/*` 白名单原样反代通道

> 本文所有字段名、路径、header、量级均来自源码精读，与仓库自带 `docs/Prism完整调用链.md`、`docs/协议校准报告.md` 交叉一致。

---

## 1. 上游调用链（逐端点：method + path + header + JSON 结构）

### 1.1 端点常量表（`internal/prism/types.go:22-66`）

```go
const (
	PathProjects            = "/api/projects"
	PathProjectAccess       = "/api/project-access"
	PathConversationHistory = "/api/codex/conversation-history"
	PathResponseStart       = "/api/llm/response_with_tools_start"
	PathResponseStatus      = "/api/llm/response_with_tools_status"
	PathResponseStop        = "/api/llm/response_with_tools_stop"
	PathSandboxNew          = "/api/backend/1/new"
	PathResourceToken       = "/api/projects/%s/sandbox/resources-token"  // fmt.Sprintf 填项目 uuid
	PathYSweetToken         = "/api/y"
	// 相对沙箱 URL 拼接（sandboxPath 只取 path，丢掉 scheme/host）
	PathSandboxToken         = "token"
	PathSandboxResourceToken = "resources-token"
	PathSandboxWaitForSync   = "wait-for-sync"
	PathSandboxRender       = "/s/sandboxes/proxy/render"
	PathSandboxRenderStatus = "/s/sandboxes/proxy/render-status"
	PathProjectFilesUpload  = "/api/project-files/upload"
	PathAuthSession         = "/api/auth/session"
)
```

**路径纠错（types.go:18-21）**：早期逆向文档写 `/api/lim/` 是笔误。实测 `/api/lim/...` 返回 Next.js 的 404 HTML（与随机不存在路径完全相同），而 `/api/llm/...` 返回真实 JSON 校验错误。是 **llm**。

### 1.2 公共请求头（`internal/prism/client.go:144-186`，`buildHeaders`）

```
Accept: <调用方指定>
Content-Type: application/json（或 multipart/form-data; boundary=...）
User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 ... Chrome/131.0.0.0 Safari/537.36
Accept-Language: zh-CN,zh;q=0.9,en-US;q=0.8,en;q=0.7
Origin: https://prism.openai.com
Referer: https://prism.openai.com/
Sec-Fetch-Dest: empty
Sec-Fetch-Mode: cors
Sec-Fetch-Site: same-origin
sec-ch-ua: "Chromium";v="131", "Not_A Brand";v="24"
sec-ch-ua-mobile: ?0
sec-ch-ua-platform: "Windows"
Cache-Control: no-cache
Pragma: no-cache
Cookie: <EffectiveCookie()>              # 见 §1.8
Authorization: Bearer <AccessToken>      # 有 access_token 才发
oai-account-id: <Cred.AccountID>
<账号级 ExtraHeaders，如 openai-sentinel-token>
```

沙箱代理端点额外头（`client.go:1448-1450`，`sandboxHeaders`）：

```go
func sandboxHeaders(sb *Sandbox) map[string]string {
	return map[string]string{"X-Crixet-Sandbox-Token": sb.Token}
}
```

> **双重认证**（client.go:316-318, 1443-1447）：沙箱端点 = Cookie（会话，cookie 在 buildHeaders 注入）**+** `X-Crixet-Sandbox-Token`。只带后者 → 直接 401 且**响应体为空**，极难排查。这是实测踩过的坑。

### 1.3 `POST /api/backend/1/new` — 申请沙箱

请求体：`{}`（空对象）

响应（`Sandbox`，types.go:405-419）：

```json
{"url":"https://prism.openai.com/s/sandboxes/proxy","token":"gAAAAAB..."}
```

```go
type Sandbox struct {
	URL       string `json:"url"`
	Token     string `json:"token"`
	ID        string `json:"sandbox_id"`          // 实测当前不返回（可选）
	SessionID string `json:"sandbox_session_id"`  // 实测当前不返回，但签发资源令牌是合法入参
}
func (s *Sandbox) Usable() bool { return s != nil && s.URL != "" && s.Token != "" }
```

**不绑定项目**：同一 token 可用于任意项目的会话，所以缓存粒度是「按账号」（`sandboxCache.Get(accountID)`）。

### 1.4 `GET <sandbox>/wait-for-sync?wait_ms=10000` — 等工作区同步

- header：`X-Crixet-Sandbox-Token: <token>`（外加 Cookie）
- query：`wait_ms=10000`（固定 10s 长轮询）

响应（`SandboxSyncStatus`，types.go:485-505，`client.go:1300-1334`）：

```json
{
  "status": "syncing | synced | failed",
  "readinessCapabilities": ["current_y_sweet_provider"],
  "tokens": {
    "hasResourceToken": false,
    "hasResourceBaseUrl": false,
    "hasResourceProjectId": false,
    "hasCurrentYSweetToken": true,
    "hasSyncedYSweetProvider": false,
    "fileCredentialSource": "..."
  }
}
```

**readinessCapabilities 的 nil vs 空数组语义不同**（这是照抄前端 `lr()` 的分支，types.go:489-536）：

| readinessCapabilities | 就绪判定 |
|---|---|
| `nil`（字段缺失） | 旧协议：`status=="synced" && Tokens.HasCurrentYSweetToken` |
| 不含 `current_y_sweet_provider` | 同上 |
| 含 `current_y_sweet_provider` | 新协议：上述 **且** `Tokens.HasSyncedYSweetProvider` |

```go
func (s *SandboxSyncStatus) Ready() bool {
	if s.Status == "failed" { return false }
	if !s.Tokens.HasCurrentYSweetToken { return false }
	if s.ReadinessCapabilities == nil { return s.Status == "synced" }
	if !s.NeedsYSweetProvider() { return s.Status == "synced" }
	return s.Status == "synced" && s.Tokens.HasSyncedYSweetProvider
}
```

`WaitSandboxReady` 的状态机（client.go:1289-1335）：
- `404` / `501` → 视为「无需同步」，直接通过
- `200` + JSON 解析成功 → 判 `Ready()`；`status=="failed"` 提前放弃
- `4xx`（除 408/429）→ 请求本身不对，放弃等待
- **网络错误（含 TLS 被直接掐断 → EOF）视为「还没好」，继续等**（未就绪时该端点会直接断 TLS，这是正常现象）
- 每次成功后 `SleepCtx(ctx, 1s)` 再轮询

### 1.5 `POST /api/projects/{project_uuid}/sandbox/resources-token` — 签发资源令牌

请求体（client.go:1345-1362）：

```json
{"sandbox_session_id": null, "sandbox_token": "<沙箱 token>"}
```

（`nilIfEmpty`：空字符串序列化成 `null` 而非 `""` —— 上游对 `sandbox_session_id` 的校验接受 null，空字符串可能被判非法）

响应（`ResourceToken`，types.go:426-467）：

```json
{
  "access_token": "eyJ...",                              // HS256 JWT，claims 带 project_uuid
  "resources_base_url": "https://prism.openai.com/s/sandbox-resources",
  "expires_at": 1789609321,                              // unix 秒
  "max_age_seconds": 3600
}
```

`Expiry()` 取 `expires_at` 与 `now+max_age_seconds` 的**更早者**（保守估计）。有效期 1 小时且**绑定单个项目**，所以缓存粒度必须是 (账号, 项目)。

### 1.6 `POST <sandbox>/resources-token` — 交资源令牌给沙箱

请求体（client.go:1369-1389）—— **字段名大小写是上游的，不要顺手统一**：

```json
{"token": "<access_token>", "resourceBaseUrl": "<resources_base_url>", "projectId": "<projectID>"}
```

`resources_base_url` 缺失时按前端规则推导（`sandboxResourcesBaseURL`, client.go:1455-1465）：
`/s/sandboxes/proxy → /s/sandbox-resources`，`/sandboxes/proxy → /sandbox-resources`。

### 1.7 `POST /api/y` — 取 Y-Sweet 凭证；`POST <sandbox>/token` — 原样转交

请求体（client.go:1400-1409）：

```json
{
  "docId": "<projectID>",
  "requestContext": {
    "source": "initial-bootstrap",
    "requestSeriesId": "oaiprism-<projectID>",
    "maxAttempts": 5
  }
}
```

响应（`YSweetToken`，types.go:474-483）：

```json
{"docId":"...","url":"wss://.../y/d/<uuid>/ws","baseUrl":"https://.../y/d/<uuid>","authorization":"...","token":"..."}
```

**转发时发 `tk.Raw`（原始 JSON 字节），不是结构体**（client.go:1423-1424, 1432-1441）—— 这样上游新增字段不会被我们的结构体吃掉。实测交付成功响应 `{"success":true,"message":"Token received"}`；交付成功后 `wait-for-sync` 立刻从 `syncing` 变 `synced`。

> **关键结论**：第 4 步之后是**沙箱自己**连 Y-Sweet 同步文档，Go 侧不需要实现 Yjs/lib0 —— 因此**零 CRDT 依赖**（实测结论，非推断）。

四步完整链（`runner.go:665-753`, `syncSandboxWorkspace`）：

```
1. 后端签发资源令牌   POST /api/projects/{id}/sandbox/resources-token
2. 把资源令牌交给沙箱  POST <sandbox>/resources-token      (header: X-Crixet-Sandbox-Token)
3. 取 Y-Sweet 凭证     POST /api/y
4. 原样交给沙箱        POST <sandbox>/token                (header: X-Crixet-Sandbox-Token)
5. 等同步完成          GET  <sandbox>/wait-for-sync?wait_ms=10000
```

### 1.8 凭据拼装（`internal/creds/creds.go`）

Prism **不走** next-auth 的 `__Secure-next-auth.session-token`（虽保留兼容名）。实测 Prism 有自己的 cookie 集（creds.go:28-53）：

```
prism_oai_access_token          ← 真正的 Bearer JWT（最关键）
prism_session_token             ← Prism 自己的会话 JWT（HS256，约 12h）
prism_oai_refresh_token         ← OAuth refresh token（rt.1.*，约 90 天）
prism_oai_earliest_refresh_at   ← 允许刷新该 token 的最早时间戳
prism-did / oai-did             ← 设备标识
cf_clearance / __cf_bm          ← Cloudflare
```

`EffectiveCookie()`（creds.go:137-173）按序拼：`SessionCookieName=SessionToken` → `prism_oai_refresh_token` → `oai-did=AccountID`。`SessionCookieName` 记录值来自哪个 cookie 名 —— 名字写错 = 上游认为「没有会话」，表现为莫名其妙的 401。

有 JWT 时 `applyJWT`（creds.go:264-300）**就地解析不验签**，从 claims 取 `exp`、`email`、`sub`、`https://api.openai.com/auth.chatgpt_account_id`、`chatgpt_plan_type`，省掉一次 `/api/auth/session` 往返。

### 1.9 `POST /api/llm/response_with_tools_start`

请求体（`buildStartPayload`, client.go:533-596；实测来自前端 bundle）：

```json
{
  "input": [
    {"type":"message","role":"system","content":[{"type":"input_text","text":"..."}]},
    {"type":"message","role":"user","content":[{"type":"input_text","text":"..."}]},
    {"type":"message","role":"assistant","content":[{"type":"input_text","text":"..."}]}
  ],
  "previousResponseId": "<上一轮 request_id>",   // 留空则彻底不发这个字段
  "conversationId": "<会话 ID>",                 // camelCase！
  "metadata": {
    "model": "gpt-6.1-sol",
    "reasoning_effort": "low|medium|high|xhigh",
    "userId": "<调用方身份>",
    "projectId": "<项目 uuid>",
    "frontend_origin": "https://prism.openai.com",
    "sandbox_url": "<步骤①的 url>",              // snake_case，必须
    "sandbox_token": "<步骤①的 token>",          // snake_case，必须
    "tools": [{"type":"function","function":{"name":...,"description":...,"parameters":{...}}}]
  }
}
```

要点：
- `input` **恒为数组**，不是数组上游直接回 `input must be an array`
- `metadata` 是**开放容器**，模型参数与运行上下文全在里面，**不在请求体顶层**
- `input` 数组**接受 `system` 角色**（前端源码第一条 input 就是 `role:"system"`）—— 不要把系统提示折成 user 消息，那等于把「指令」降级成「用户发言」
- `tools` 上游不认（真实前端请求体里根本没有它），默认塞进 metadata，源码明确标注为**待验证**
- 字段名大小写上游自己不一致：start 用 camelCase `conversationId`，status 用 snake_case `request_id`，**照抄不要统一**（client.go:610-612 注释）

响应（`PrismEnvelope`, types.go:384-396）：

```json
{"status":"started","request_id":"<uuid>","conversation_id":null,"turn_state":{...}}
```
或直接终态：
```json
{"status":"completed","request_id":"<uuid>",
 "response":{"status":"success","payload":{
   "id":"...","conversationId":"...",
   "output":[{"type":"message","role":"assistant","status":"completed",
              "content":[{"type":"output_text","text":"..."}]},
             {"type":"reasoning","summary":[{"type":"summary_text","text":"..."}]}],
   "codexDeltaFiles":[{"file_path":"main.tex","status":"added|modified|deleted","diff":"..."}],
   "usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}}
```
失败也走 `"completed"`，但 `response.status == "error"`：
```json
{"status":"completed","response":{"status":"error",
 "payload":{"reason":"sandbox_reconnecting","message":"...","rootCause":"...","httpStatus":504}}}
```

> **本协议最容易踩的坑**（client.go:495-496）：失败是 **HTTP 200 + `response.status:"error"`**。只看 HTTP 状态码会把失败当成功，返回一个「成功的空回答」。

### 1.10 `POST /api/llm/response_with_tools_status` — 轮询

请求体**只有两个字段**（client.go:620-636, `buildStatusPayload`）：

```json
{"request_id":"<uuid>","turn_state":{...}}
```
（可选加 `"waitMs": 10000`，由 `facade.use_status_wait` 控制；上游当前不认，发出去无副作用也无收益）

**turn_state 回传规则（全协议核心）**：
- `turn_state` 是服务端下发的**不透明状态对象**，逐轮换新
- **必须原样回传** —— 自己构造任意值都会被拒成 `"turn_state is required"`
- Go 侧一律用 `json.RawMessage` 保存，从不解析其内部结构
- **忘了更新 turn_state 会一直拿到同一个 pending，表现为「永远不结束」**（runner.go:515-516）
- 校验证据（config.go:571-573）：空体 → `request_id is required`；带 request_id → `turn_state is required`

轮询响应与 start 同构（`status: started|pending|completed`）。

### 1.11 `POST /api/llm/response_with_tools_stop` — 取消

请求体（client.go:661-688）：

```json
{"request_id":"<uuid>","conversation_id":"<conv>","turn_state":{...}}
```

用途（client.go:657-660）：**客户端断开时若不通知上游，那条生成会继续跑完并消耗额度**。对「额度即成本」的代理来说这是最直接的省钱手段。实现要点（runner.go:599-611）：用**独立的 5s 超时 context**（`context.Background()`），因为主 context 已取消，不能拿它去发请求。

### 1.12 项目与文件端点

| 端点 | 请求体 / 说明 |
|---|---|
| `POST /api/projects` | `{"project_uuid":"<客户端生成>","title":"oaiprism"}` —— 字段名来自上游 400 校验错误。`project_uuid` 是**客户端生成的幂等键**，重发同一 uuid 拿回同一项目 |
| `GET /api/project-access?d={UUID}` | → `{"has_access":bool,"access":"...","role":"...","expires_at":"..."}` |
| `POST /api/codex/conversation-history` | 历史可能挂在 `messages`/`history`/`turns`/`items`/`conversation` 下 |
| `POST /s/sandboxes/proxy/render` | 202 异步；body 同时发 `project_id`+`projectId`、`file_id`+`fileId`（不猜一个，两个都带）；job id 也可能在 `Location` 头 |
| `GET /s/sandboxes/proxy/render-status?waitMs=10000` | 三个参数名 `id`/`job_id`/`render_id` 全带 |
| `POST /api/project-files/upload` | multipart：`project_id`+`projectId`、`file_id`+`fileId`、`path`、`file`；另带头 `x-prism-file-id` / `x-prism-project-id` / `x-prism-file-name`(url 转义) / `x-prism-file-size` |
| `PATCH /api/projects/{uuid}/thumbnail` | 缩略图 |

### 1.13 解析层策略：「精确优先、宽容兜底」

`parseStatus`（client.go:707-737）→ `parseEnvelope`（758-969，强类型 `PrismEnvelope` 确定性解析）→ 认不出才 `parseGeneric`（1079-1152，BFS 宽容抽取）。

为什么要这个顺序（client.go:704-706）：宽容解析对「错误」的识别是启发式的，而本协议恰好用 HTTP 200 + 嵌套 error 表达失败 —— **只有精确解析才能可靠区分「成功但还没输出」与「已经失败了」**。

两个刻意的抽取选择（`extractCodexOutput`, client.go:971-1041）：
- `payload.output` **只取最后一条有内容的 assistant 消息**（倒序 + 跳过空文本）：多轮会话下 output 会累积历史，全拼会把之前的回答也当成这一轮结果；直接取 `arr[len-1]` 会拿到夹在尾部的空工具条目
- 跳过 `input_image`/`input_file`（那是用户输入的回显，不是回答）；`refusal` 保留（是回答内容）

### 1.14 增量还原：`Diff`（`internal/prism/extract.go:245-277`）

```go
func Diff(prev, cur string) (string, bool) // 返回 (delta, reset)
```
- `cur == prev` → `("", false)`
- `strings.HasPrefix(cur, prev)` → `(cur[len(prev):], false)`
- 非前缀 → 按公共前缀裁剪 + `reset=true`（上游重写了整段，上层按「覆盖」语义处理）

**这是把「轮询式协议」转成「流式协议」的核心技巧**：不管上游返回累计全文还是结构化消息列表，只要单调增长，就能用前缀差分还原 token 级增量。

---

## 2. 沙箱

### 2.1 冷启动与 504

- 沙箱申请 = 真的分配一个容器，**很慢**，必须缓存复用（`facade.sandbox_ttl` 默认 30min）
- **沙箱预热**：申请到沙箱后立刻 start，会等约 **122 秒**然后返回
  ```
  reason: "unknown"
  message: "Error while processing conversation (504 Gateway Timeout). Please submit prompt again."
  ```
  上游自己建议 `Please submit prompt again` —— **这是上游建议的处理方式，照做即可，不要当成协议错误**（runner.go:356-357）
- **漏掉沙箱的误导性症状**：start 立刻返回 `status:"completed"` + `response.status:"error"`、`reason="sandbox_reconnecting"`，看起来像「上游挂了」，实际上是「你没给我沙箱」。上游 `codexRequestDebug` 里会直接写 `sandbox_url_resolved: null`（types.go:29-35）
- **只申请沙箱不够**：拿到句柄后仍停在 `syncing`，必须再注入项目资源令牌 + Y-Sweet 凭证。漏掉这一步的症状：**不报错，只是永远 syncing，最终会话处理固定 122 秒后 504**（types.go:40-43, runner.go:315-321）

### 2.2 沙箱未就绪的重试策略（`isSandboxNotReady`, runner.go:771-796）

判定为「沙箱没准备好」而非真失败的信号：

```go
case reason == "sandbox_reconnecting",
	reason == "sandbox_not_ready",
	strings.Contains(msg, "submit prompt again"),
	strings.Contains(msg, "reconnecting to sandbox"),
	strings.Contains(msg, "gateway timeout"):
```

重试参数（runner.go:613-626）：

```go
sandboxStartRetries = 10                   // 3 → 10（2026-09-17）
sandboxRetryDelay   = 5 * time.Second      // 线性递增：5s,10s,...,50s
```
调参理由（源码注释原文）：Codex CLI 在流断后会自动重连约 **12 分钟**（5 次 × 2m23s），此前服务端只重试 15 秒就放弃 —— **上游限流窗口是分钟级，两边窗口严重错配**。10 次 × 线性退避约 8-9 分钟，基本覆盖 CLI 的重连窗口。**每次重试前先 `sandboxes.Invalidate(acct.ID)`**（大概率是容器被回收了）。

### 2.3 两层缓存粒度刻意不同（`internal/facade/sandbox.go`）

| 缓存对象 | 粒度 | 理由 |
|---|---|---|
| 沙箱本身 | **(账号)** | 一个容器能服务该账号下多个项目 |
| 工作区同步状态 | **(账号, 项目)** | 资源令牌绑定单个项目（project_uuid 编码在 JWT 里）且只有 1h |

```go
type sandboxEntry struct {
	sb       *prism.Sandbox
	expires  time.Time
	projects map[string]time.Time   // projectID -> 该次同步所用资源令牌的过期时刻
}
```

**用资源令牌的过期时间当缓存失效点，而不是自己拍一个 TTL**（sandbox.go:34-36）：令牌过期后沙箱就读不到项目资源了，再发请求必然失败，与其等失败再重试，不如到点主动重同步。

`Put` 的幂等细节（sandbox.go:62-82）：若已有条目且**是同一个沙箱 token**，保留其 `projects` 同步记录 —— 并发申请撞车时不该把已经做好的同步成果丢掉。

`Invalidate`（丢整个账号缓存，容器被回收时）vs `InvalidateProject`（只让某项目同步记录失效，保留沙箱，重试时不必再花一次容器分配）。

### 2.4 并发去重锁

- `Lock(accountID)` — 同一账号只允许**一个在飞的沙箱申请**，避免并发请求各申请一个沙箱
- `LockProject(accountID, projectID)` — 同项目只允许一个在飞的同步；并发相同请求应**等**前一个完成，而不是各自去签一份资源令牌（**那会同时开多个沙箱会话，白耗额度**）

`ensureSandbox` **不等就绪**（runner.go:655-659）：容器分配完成只是必要条件，真正「就绪」要等工作区同步，而那一步需要 projectID，`ensureSandbox` 拿不到。把等待放在那里只会白等一个窗口。

### 2.5 失败降级原则

沙箱申请失败、工作区同步失败、建项目失败 —— **都不阻断 start**（runner.go:288-328）。理由：真失败了 start 会给出明确原因，比在这里提前判死更有信息量。**唯一的例外**是 `ctx.Err() != nil`（上下文取消是终态，不是可降级继续的故障）：把它混进降级分支会拿着已取消的 ctx 继续发 start，白跑一趟还在日志里留下误导性记录（runner.go:284-290）。

---

## 3. 流式：上游非真流，代理如何合成 delta

### 3.1 核心事实：上游没有真流

上游只有 start + poll，`status` 端点在 `pending` 帧是否携带累计正文**未知**（前端自己在 pending 分支只读 `turn_state`、完全不看 `response` —— 这暗示 pending 帧可能根本没有正文，那样「流式」实质上是一次性给出，config.go:638-640）。

代理的做法：**每轮 poll 都做前缀差分（`Diff`），有增量就立即 emit** —— 上游给累计正文时就是准流式，不给时最坏退化成一次性输出，协议层无差别。

### 3.2 `internal/sse/writer.go` — 写出行

- 预编码常量帧：`DoneFrame = []byte("data: [DONE]\n\n")`、`CommentPing = []byte(": ping\n\n")`
- `bufPool sync.Pool`：`make([]byte, 0, 8192)`，`Close()` 时若 `cap<=1MB` 归还 → **稳态零分配**
- 响应头（writer.go:86-96）—— `X-Accel-Buffering: no` 不是可选项：
  ```go
  h.Set("Content-Type", "text/event-stream; charset=utf-8")
  h.Set("Cache-Control", "no-cache, no-store, no-transform, must-revalidate")
  h.Set("Connection", "keep-alive")
  h.Set("X-Accel-Buffering", "no")
  h.Set("Pragma", "no-cache")
  ```
  少了 `X-Accel-Buffering: no`，Nginx 会把整个流缓冲到结束才吐给客户端，「流式」退化成「一次性返回」，**现象是首字延迟等于整段生成时间**。
- `mu sync.Mutex` 加锁使写路径**并发安全**：长等待场景需要「心跳 goroutine + 业务 goroutine」同时写（writer.go:49-53）
- `flushBuffer`：**Flush 是流式的生命线**，不 Flush 的话 net/http 会攒到 4KB 才发，表现为「回答卡顿、一段一段地蹦」
- `WriteData` 检查裸换行（SSE 规范要求 data 字段内不能出现裸换行），多行拆成多个 `data:` 行
- 手写 JSON 编码（writer.go:237-330）：`AppendJSONString` / `AppendInt` / `AppendFloat`
  - 不做 HTML 转义（`< > &` 保持原样 —— SSE 消费方是 JSON 解析器，不是 `<script>` 标签），多余的转义只增加体积
  - ASCII 快路径；非法 UTF-8 → `U+FFFD`；`\u2028`/`\u2029` 转义（作为 JS 行分隔符会出问题）
  - 实测单 chunk 编码开销下降一个数量级

### 3.3 保活：15 秒心跳

```go
const heartbeatInterval = 15 * time.Second   // responses.go:426
```

`streamResponses` 起一个 goroutine，每 15s 发一个 `response.in_progress` 事件（responses.go:148-180）。**为什么必需**（源码注释原文）：start+poll 一轮可能 1-5 分钟（上游沙箱重试、xhigh 长推理），期间若一个字节都不发，中间链路（node sidecar、反代、Nginx `proxy_read_timeout`）会按空闲掐连接，客户端表现为 `"stream closed before response.completed"`。实测 **Codex CLI 0.154/0.159 对长时间静默同样会判流断**。

15s 的取值理由：远小于常见反代默认 60s，又不会显著增加事件量（一轮 5 分钟约多 20 个事件，可忽略）。

### 3.4 Chat / Anthropic / Responses 三条流的合成差异

**Chat Completions**（chat.go:124-242）：
- 首帧**必须是 role**（`{"delta":{"role":"assistant"}}`），否则部分严格客户端认为格式非法
- `emit` 把 `d.Reasoning` 与 `d.Text` **分成两个 chunk** —— 混在一起会让客户端把推理过程当正文渲染
- 结束帧带 `finish_reason`（`tool_calls` 或 `stop`）
- `include_usage` 时补一帧 `EmptyChoices:true` 的 usage 帧（choices 为空数组、只带 usage）

**Anthropic**（anthropic.go:120-176）：`message_start`（此时还不知道 input_tokens，用占位值）→ `content_block_start` → N×`content_block_delta` → `content_block_stop` → `message_delta`（给准确 output_tokens + `stop_reason:"end_turn"`）→ `message_stop`

**Responses**（responses.go:123-304）：
- 序言：`response.created` → `response.in_progress` → `response.output_item.added` → `response.content_part.added`
- 增量：`response.output_text.delta`
- 收尾：`response.output_text.done` → `response.content_part.done` → `response.output_item.done` → `response.completed`（**收尾事件必须逐个发全，否则 SDK 会一直等 response.completed**）
- 错误：`response.failed`（**不是自定义 `error` 事件**）。为什么必须有它（encode.go:317-325）：只发自定义 `error` 时，Codex CLI 状态机等不到任何**终止**事件（它只认 completed / failed / incomplete），于是流一结束就报 `stream closed before response.completed` —— 用户看到「流莫名断了」，真正的失败原因（上游 504/沙箱未就绪）完全丢失。实测于 Codex CLI 0.154/0.159

### 3.5 错误中断与客户端断连取消

- 流式响应头已经发出，**没法再改状态码** → 按 SSE 惯例发一个错误载荷，再以 `[DONE]` 收尾，让客户端至少拿到明确失败信号而不是超时（chat.go:174-187）：
  ```go
  buf = append(buf[:0], `{"error":{"message":`...)
  buf = sse.AppendJSONString(buf, runErr.Error())
  buf = append(buf, `,"type":"upstream_error"}}`...)
  ```
- **客户端断连 → 立即取消上游**（`response_with_tools_stop`）：轮询循环里每次检查 `ctx.Err()`，非 nil 就 `stopUpstream(...)` 再返回（runner.go:466-470）。emit 回调返回 error（下游写失败）同样触发 stop（runner.go:536-539）
- `mapError`（chat.go:344-371）把内部错误映射成 HTTP 状态码，其中一条容易搞错：**上游 401 不原样透传成 401**（那会让客户端以为自己的 API Key 有问题并反复重试），对调用方这是「网关的上游凭据挂了」，属于 **502**。`context.Canceled` → **499**（Nginx 约定的「客户端主动断开」）

### 3.6 轮询节流（一个被测试抓出来的 DoS bug）

`runner.go:559-574` 的注释值得整段抄：

> 这里必须是「无条件节流」，不能只在空轮询时睡：如果上游每轮都返回累计正文（也就每轮都有增量），「有增量就立刻再问一次」会让循环退化成**零间隔的忙轮询** —— 4 次轮询 20 毫秒跑完，等于在对上游做拒绝服务。（这个 bug 是被「客户端断开后应通知上游停止」的测试顺带抓出来的。）

规则：本轮耗时已经 >= 目标间隔，说明服务端本身就在等（长轮询语义），直接进下一轮；否则补足差值。

其他轮询参数：`poll_interval=1s`（上游 429 时优先调大）、`poll_backoff_max=3s`（空轮询时指数退避，有进展就把退避重置回基线）、`max_poll_timeout=15min`、`sync_timeout=10min`（同步模式收紧，不能让一个 HTTP 请求挂 15 分钟）。轮询连续失败 6 次放弃；账号级错误立即返回不重试。

---

## 4. 工具桥接（toolbridge.go + toolcall.go）

### 4.1 背景（toolbridge.go:10-25 原文）

> 2026-09-17 实测定论：上游是 server-side tools 架构，模型的终端/文件工具**在云端沙箱执行并消化**，客户端永远只拿到最终文本 —— 所以本地 CLI 的工具链（exec_command 等）**一次也不会被触发**，模型「创建」的文件全部留在云端容器里，用户磁盘上什么都没有。

桥的思路：

```
上游当大脑，本地 Codex CLI 当手脚：
① system 指令里明确「你没有任何执行环境」
② 要求它把所有操作以 ```codex-exec 围栏（内含一段 JS，调用 exec_command）输出
③ 代理解析这段 JS，包装成 Responses 协议的 custom_tool_call 返回
④ Codex CLI 在本地 V8 isolate 里执行它（exec_command 跑真命令，文件落在用户磁盘）
⑤ 把结果回传，代理翻译成文本继续下一轮
```

> 桥 prompt 必须**显式抑制上游自带的沙箱工具**，否则模型仍会在云端执行然后「汇报成功」—— 用户看到一切正常，本地空空如也。

### 4.2 触发检测（`BridgeEnabled`, toolbridge.go:31-37）

```go
func BridgeEnabled(raw map[string]json.RawMessage) bool {
	rawInput, ok := raw["input"]
	if !ok || len(rawInput) == 0 { return false }
	return strings.Contains(string(rawInput), `"additional_tools"`)
}
```
Codex CLI 把工具声明放在 `input[0]`（`type=additional_tools`），**顶层 `tools` 为 null** —— 所以不能用顶层 `tools` 判断。

### 4.3 提示词协议（`bridgePrompt`, toolbridge.go:43-76，节选）

```
<local_tool_bridge>
You are the reasoning engine for a LOCAL coding agent (Codex CLI). The client executes ALL tools locally on the user's machine.

CRITICAL: You have NO terminal, NO file system, and NO sandbox tools in this conversation. Any built-in shell/codex/terminal tools in your runtime operate in a REMOTE SANDBOX the user cannot see. NEVER use them. NEVER claim you created, ran, or modified anything unless the client's tool result (marked [CLIENT RESULT]) confirms it.

To run any command or create/edit/delete files on the user's machine, output EXACTLY ONE fenced block:
```codex-exec
const out = await tools.exec_command({ cmd: "..." });
text(out);
```

The block content is raw JavaScript executed by the client in a V8 isolate:
- `tools.exec_command({ cmd: string, max_output_tokens?: number })` runs one shell command in a PTY and returns its output (string).
- The client shell on Windows is PowerShell; on macOS/Linux it is bash.
- `text(value)` appends a result for the model to read; `exit()` ends the script.

Command recipes ...
- Create/overwrite a file, Windows/PowerShell:
  $c = @'<FULL FILE CONTENT>'@; Set-Content -LiteralPath '<path>' -Value $c -NoNewline
- Create/overwrite a file, macOS/Linux/bash:
  cat > '<path>' <<'EOF'<FULL FILE CONTENT>EOF
- NEVER use bash-only syntax when the client is Windows — it fails silently and wastes a turn.

Output rules: outside the block write at most one short sentence of prose. ... Always emit the FULL file content in the command — never abbreviate.
Do NOT emit a block for greetings, questions, or small talk, and do NOT run environment checks or "test" commands (like true/echo/ls) to probe the client.
</local_tool_bridge>
```

### 4.4 JSON 下单与消息序列构造（`bridgeInputItems`, toolbridge.go:84-174）

把 Codex CLI 的 input 数组翻译成上游 input。**与普通 messages 翻译的关键区别**：工具条目必须**保留为文本**，上游需要看到它上一轮「发出」的指令和客户端的执行结果，否则每轮都会重新规划已经做过的操作。

```go
items = append(items, prism.NewSystemItem(defaultSystem + "\n\n" + bridgePrompt()))
...
case "custom_tool_call", "function_call":
	// 上游「上一轮」发出的调用：以它原始的样子回放
	items = append(items, prism.NewAssistantItem(
		"```codex-exec\n"+textOf(b.Input)+"\n```"))
case "custom_tool_call_output", "function_call_output":
	header := "[CLIENT RESULT]"
	if b.CallID != "" || b.Name != "" {
		header = "[CLIENT RESULT"
		if b.CallID != "" { header += " call_id=" + b.CallID }
		if b.Name != ""   { header += " tool=" + b.Name }
		header += "]"
	}
	items = append(items, prism.NewUserItem(
		header+"\n"+textOf(b.Output)+"\n[/CLIENT RESULT]"))
```
`additional_tools` / `reasoning` / 其它非消息条目：跳过。

**末尾重申指令**（toolbridge.go:168-172）—— 这是被实测逼出来的：

> LLM 对序列末尾的指令服从度最高 —— 桥指令只放在开头会被 CLI 传入的 Codex 人设（4 条 developer 消息，要求「使用 exec 工具」）压过去：**实测模型无视开头的桥指令，直接在云端沙箱里执行并口头汇报「已创建」**。末尾重申一次。

### 4.5 解析与回灌（`extractExecBlock` + `ensureExecJS`）

```go
func extractExecBlock(text string) (string, bool) {
	const fence = "```codex-exec"
	idx := strings.Index(text, fence)
	if idx < 0 { return "", false }
	rest := strings.TrimLeft(text[idx+len(fence):], "\r\n")
	end := strings.Index(rest, "```")
	if end < 0 {
		// 未闭合：把剩余部分整体当作块内容（流式截断时可能发生）
		rest = strings.TrimRight(rest, "`")
	} else {
		rest = rest[:end]
	}
	js := strings.TrimSpace(rest)
	if js == "" { return "", false }
	return js, true
}
```

**`ensureExecJS` 是代理层兜底**（toolbridge.go:234-250）：

```go
func ensureExecJS(candidate string) string {
	if strings.Contains(candidate, "tools.") || strings.Contains(candidate, "await") {
		return candidate // 已经是 JS
	}
	// 不含 JS 特征的内容视为一条 shell 命令，自动包上 exec_command
	return `const __out = await tools.exec_command({ cmd: ` + jsonStr(candidate) + ` });
text(__out);`
}
```
理由（源码注释）：实测模型经常无视「输出 JS」的要求、直接把 shell 命令写进围栏 —— 那样进了 V8 就是 SyntaxError，然后进入「语法错误→模型困惑→换个姿势再错」的死循环。**与其反复纠正模型，不如代理层兜底**。

### 4.6 失败处理：自动纠正一轮（`bridgeRetryNudge`）

模型没用桥格式时（大概率在云端沙箱里执行后口头汇报），自动纠正**一轮**（只重试一次，避免循环，responses.go:207-228）：

```
[SYSTEM CORRECTION] Your previous reply did NOT contain a ```codex-exec ``` block, so NOTHING was executed on the user's machine. Whatever you did with your built-in tools ran inside a remote container that the user cannot see or access.
Emit the ```codex-exec ``` block NOW with the full command (including the complete file content) so the client can execute it locally. Do not re-describe the task — output the block.
Your previous reply was: "<prevText 截断 300 字符>"
```
关键（toolbridge.go:178-179）：**要打破模型的错觉** —— 它在云端沙箱里真的执行成功了，所以它坚信任务已完成，必须明确告知那个执行对用户不可见。

注意：**桥模式不能边收边发**（responses.go:182-184）—— 必须先拿到完整回复才能判断它是工具调用（` ```codex-exec ` 块）还是纯文本。缓冲后统一输出。

回灌为 Responses 协议的 `custom_tool_call`（toolbridge.go:256-266）：

```go
func customToolCallItemJSON(id, js string, index int) string {
	return `{"id":` + jsonStr(id) +
		`,"type":"custom_tool_call","status":"completed","call_id":` + jsonStr(id) +
		`,"name":"exec","input":` + jsonStr(js) + `}`
}
```
Codex 0.15x 的工具是 `type=custom`（name=exec，input 为自由 JS 源码），**不是 function** —— input 直接是源码字符串，不带 arguments 包装。

`writeJSONString` 用 `Encoder + SetEscapeHTML(false)`：`json.Marshal` 默认把 `< > &` 转成 `\u003e` 等，对 CLI 功能无影响，但会让 exec JS 源码面目全非、难以排查。

### 4.7 parallel 与「DeltaFiles 通道」两条路（toolcall.go）

`oai-prism` 实际有**两套**工具产出路径：

**A. 工具桥（toolbridge.go）** — Codex CLI 场景，模型输出 ` ```codex-exec ` JS，代理包成 `custom_tool_call`

**B. DeltaFiles → 标准 ToolCalls（toolcall.go）** — 上游沙箱内 Codex 真实执行的文件变更集（`codexDeltaFiles`），代理把它转成客户端期望的标准 `tool_calls`（toolcall.go:61-128）：

```go
preferredWriteTool := "write_to_file"; preferredEditTool := "edit_file"
// 探测客户端声明了哪些文件编辑工具名
for _, t := range declaredTools {
	name := strings.ToLower(strings.TrimSpace(t.Function.Name))
	if name == "write_to_file" || name == "create_file" || name == "write_file" || name == "new_file" {
		preferredWriteTool = t.Function.Name; hasDeclaredWrite = true
	} else if name == "edit_file" || name == "apply_diff" || name == "str_replace_editor" || name == "patch" || name == "modify_file" {
		preferredEditTool = t.Function.Name; hasDeclaredEdit = true
	}
}
// status: added → write_to_file{path,content}
//         modified → 若客户端只声明了 edit 工具 → edit_file{path,diff,content}；否则 write_to_file{path,content}
//         deleted → delete_file{path}
```

配套两个能力：
- `ExtractContentFromDiff`（toolcall.go:17-58）：从统一 diff 还原新增/修改后的文件内容（跳过 `---`/`+++`，从 `@@` 起进入 hunk，丢弃 `-` 行，`+` 行去前导 `+`，上下文行去前导空格）
- `ApplyLocalWorkspaceFiles`（toolcall.go:131-165）：直接把 DeltaFiles 写进本地工作区，**带路径遍历防护**：
  ```go
  targetPath := filepath.Join(cleanRoot, filepath.Clean(f.FilePath))
  if !strings.HasPrefix(targetPath, cleanRoot) {
      return fmt.Errorf("非法路径越界: %s", f.FilePath)
  }
  ```
  由请求头 `X-Local-Workspace` 触发（chat.go:197-203）。

`newCallID()` 生成 `call_` + 12 字节 hex（标准 OpenAI 形态）。

`finishReason`（chat.go:312-320）：`len(res.DeltaFiles) > 0` → `tool_calls`，否则 `stop`。

---

## 5. token 计量

### 5.1 上游 usage 字段（`Usage`, types.go:398-403）

```go
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}
```
解析时兼容两套命名（client.go:893-899, intOf）：
- input：`input_tokens` / `prompt_tokens`
- output：`output_tokens` / `completion_tokens`
- `TotalTokens == 0` 时用 `Input + Output` 兜底

usage 可能出现在**两个位置**：包络顶层 `usage`（client.go:890-901）或 `response.payload.usage`（client.go:938-947）。

### 5.2 上报：三套协议映射 + 指标 + 持久化

```go
// ChatUsage（types.go:167-171）
{"prompt_tokens": In, "completion_tokens": Out, "total_tokens": Total}
// AnthropicUsage（types.go:224-227）
{"input_tokens": In, "output_tokens": Out}
// ResponsesUsage（types.go:291-295）
{"input_tokens","output_tokens","total_tokens"}
```

指标（runner.go:193-196）：
```go
r.app.Tokens.With(api, "input").Add(int64(res.Usage.InputTokens))
r.app.Tokens.With(api, "output").Add(int64(res.Usage.OutputTokens))
// 指标名 oaiprism_tokens_total，标签 {api, direction}
```

SQLite 持久化（`request_logs` 表，sqlite.go:76-90 / 295-380）：列出 `prompt_tokens`、`completion_tokens` 两个 INTEGER 字段并建索引 `idx_req_time`/`idx_req_model`/`idx_req_account`。`RequestLogItem` 的字段：`id,timestamp,method,path,model,account_id,status_code,duration_ms,prompt_tokens,completion_tokens,error_message,client_ip,user_agent`。

### 5.3 估算：上游不给用量时的兜底（`estimateTokens`, chat.go:322-337）

```go
// CJK 约 1 字 1 token，拉丁文约 4 字符 1 token
func estimateTokens(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		if r >= 0x2E80 { cjk++ } else { other++ }
	}
	return cjk + other/4
}
```
只在 `res.Usage == nil` 时占位，**避免客户端拿到 null 崩掉**（chat.go:294-298）。注意：估算时 `PromptTokens: 0` —— 上游没给就归零，不猜输入侧。

### 5.4 `cached_tokens` —— **本仓库没有实现**

我用 `grep -rni "cached\|cache_read\|cache_creation\|prompt_tokens_details"` 扫过整个工作区（含文档、配置、前端）：**零命中**。

结论（供重写参考）：
- `/api/llm/response_with_tools_*` 的 usage 只有 3 个字段，**没有 `cached_tokens` / `prompt_tokens_details`**
- OpenAI 侧的 `prompt_tokens_details.cached_tokens`（0.15x SDK 会读）**由本代理直接丢弃** —— 它输出的 `ChatUsage`/`ResponsesUsage` 结构体里根本没有这个字段
- Anthropic 侧的 `cache_creation_input_tokens` / `cache_read_input_tokens` 同理，且 `AnthropicUsage` 只有 `input_tokens`/`output_tokens` 两个字段
- 这是一个**明确的可抄改进点**：即使上游不给，也可以按前缀稳定性自算（同一 `conversationId`/`projectId` 的重复 input 前缀），并在出站 usage 里补 `prompt_tokens_details.cached_tokens`（OpenAI）与 `cache_read_input_tokens`（Anthropic）

### 5.5 上下文压缩（`internal/facade/compress.go`）

```go
type CompressionConfig struct {
	MaxHistoryTurns   int  // 触发压缩的最大轮数阈值（1 轮 = 1 user + 1 assistant）
	KeepRecentTurns   int  // 保留完整原始对话的最近轮数
	MaxCharsThreshold int  // 触发压缩的最大字符数阈值
}
var DefaultCompressionConfig = CompressionConfig{
	MaxHistoryTurns: 6, KeepRecentTurns: 2, MaxCharsThreshold: 16000,
}
```

`CompressChatMessages`（compress.go:27-102）是**确定性滑动窗口 + 结构化摘要**，不是 LLM 摘要：

1. `len(msgs) <= 4` → 直接返回，不做任何处理
2. 分离 `system`/`developer` 消息（永不压缩）与非系统消息
3. 触发条件：`len(nonSys) > MaxHistoryTurns*2` **或** `totalChars > MaxCharsThreshold`（两者都未超 → 返回原样，零开销）
4. `keepCount = KeepRecentTurns*2`，边界处理：`keepCount >= len(nonSys)` 时取 `len-1`；`< 1` 时取 1
5. `splitIdx = len(nonSys) - keepCount`；`[0:splitIdx]` 压缩，`[splitIdx:]` 保留完整
6. 压缩方式：
   ```
   [Compressed Earlier Conversation Context]
   1. User: <text>
   2. Assistant: <text>
   3. Tool: <text>
   ```
   每条单条超 200 字符 → `txt[:197] + "..."`（**截断单条过长历史，避免压缩后依然膨胀**）
7. 输出 = `sysMsgs + toKeep`（系统消息 + 最近完整消息）

**摘要去哪了？** —— 这是本设计的关键一招（translate.go:36-103）：`translateChatMessages` 先调 `CompressChatMessages` 拿到 `summaryText`，然后把「最后一条 user 消息之前的所有历史」+ `summaryText` 拼成一段 `historyText`，**附挂到 system 消息的末尾**：

```go
historyText := "\n\n[Previous Conversation History]\n" + historyBuilder.String()
if !hasSystem && defaultSystem != "" {
	items = append(items, prism.NewSystemItem(defaultSystem + historyText))
}
```
理由（translate.go:53 原文）：**上游后端只提取系统 Context 和最后一条 User request 而丢弃历史** —— 所以必须把历史塞进 system 位置才不会被丢。这是绕开上游缺陷的必要变形，不是可选的优化。

---

## 6. 图片与文件通道（`facade/image.go` + `facade/project.go`）

### 6.1 `preprocessInputImages`（image.go:20-78）—— 输入图片转存

问题：客户端传的 `input_image` 是 base64 data URI 或外部 URL，**云端沙箱读不到**。做法：把图片**代上传到项目存储**，再换成上游原生识别的 `input_file`：

```go
data, ext, ok := extractImageData(ctx, c.ImageURL)
if !ok || len(data) == 0 {
	newContents = append(newContents, c)   // 拿不到就保留原样
	continue
}
filename := "image_" + randHex(6) + ext
_, err := client.UploadFile(ctx, p, prism.FileUpload{
	ProjectID: projectID, Path: filename, Filename: filename, Data: data,
})
if err != nil {
	newContents = append(newContents, c)   // 上传失败时保留原样
	continue
}
// 上传成功：转换为上游原生识别的 input_file
newContents = append(newContents, prism.InputContent{
	Type:        "input_file",
	Filename:    filename,
	ProjectPath: filename,
})
```

`extractImageData`（image.go:80-134）：
1. **Base64 data URI**：`data:image/` 前缀 → 按 `;` 前的 header 判扩展名（jpeg/jpg→`.jpg`，webp→`.webp`，gif→`.gif`，其余 `.png`）→ `base64.StdEncoding.DecodeString`
2. **外部 http/https**：**排除 `prism.openai.com` 内部域名**（避免循环抓自己），`http.Client{Timeout: 15s}`，`io.LimitReader(resp.Body, 10<<20)` 限 10MB，按 `Content-Type` 判扩展名
3. 其它 → `(nil, "", false)`

触发条件：`projectID != "" && len(items) > 0`（image.go:22）—— 无项目就没法转存，直接跳过。

### 6.2 `input_image` 的三种形态兼容（translate.go:226-258）

```go
//  {"type":"image_url","image_url":{"url":"…","detail":"high"}}   OpenAI 标准
//  {"type":"image_url","image_url":"…"}                            简化写法
//  {"type":"input_image","image_url":"…","detail":"auto"}          Responses 风格
```
`detail` 缺省回填 `"auto"`（对照 PrismOpenAIProxy transform.mjs:61,65）—— 上游若按精细度做分档计费/裁剪，留空等于交给它自己猜，行为不可预期。

**必须覆盖这些形态**（translate.go:232-233 原文）：图像 URL 若取不到，我们就会发一个**空的 input_image** 给上游 —— 上游不报错，模型只是「看不见图」，属于最难定位的一类静默失效。

### 6.3 文件通道

- 上行：`UploadFile`（client.go:1602-1657）multipart，见 §1.12；`x-prism-file-name` 做 `url.QueryEscape`
- 下行：`codexDeltaFiles` → `ApplyLocalWorkspaceFiles`（§4.7-B），`X-Local-Workspace` 头触发
- `input_file` 块字段（types.go:187-198）：`Type` + `Filename` + `ProjectPath`（`json:"project_path"`）

---

## 7. 高并发：HTTP 客户端 + 号池 + 指标

### 7.1 `internal/httpc/client.go` — 反代 HTTP 客户端

源码开头 5 条注释全部是「有意为之」的参数（client.go:1-13），逐条都有反例：

```go
tr := &http.Transport{
	Proxy:                 proxyFunc(...),
	DialContext:           dialer.DialContext,
	ForceAttemptHTTP2:     forceH2,                    // 出站固定 HTTPS，显式保证不退化回 HTTP/1.1
	MaxIdleConns:          cfg.MaxIdleConns,           // 512
	MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,    // 256（默认仅 2！）
	MaxConnsPerHost:       cfg.MaxConnsPerHost,        // 0 = 不限
	IdleConnTimeout:       cfg.IdleConnTimeout,        // 90s
	TLSHandshakeTimeout:   10 * time.Second,
	ResponseHeaderTimeout: 120 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	DisableCompression:    true,                       // 关掉自动 gzip！
	ReadBufferSize:        32 << 10,                   // 默认 4KB
	WriteBufferSize:       32 << 10,
	TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
}
```

- **`MaxIdleConnsPerHost` 默认只有 2，在高 QPS 反代下会造成大量 TIME_WAIT 与 TLS 握手，是本项目第一个要调掉的瓶颈，默认给 256**
- **`DisableCompression: true`**：Go 一旦看到客户端没设 `Accept-Encoding` 就会偷偷加 gzip 并自动解压，那会让「原样透传」失真（`Content-Encoding`/`Content-Length` 全部对不上）。**代理必须自己掌控编码协商**
- **不设 `http.Client.Timeout`**（streaming 与长轮询由 context 精确控制，全局 Timeout 会把正常的长回答误杀）
- **不跟随重定向**：`CheckRedirect` 返回 `http.ErrUseLastResponse` —— 上游 302 往往意味着登录态失效，跟过去只会拿到一个 HTML 登录页，反而掩盖真实错误
- `proxyFunc`：socks5 支持不了就 **panic 明确报错而不是静默直连** —— 静默直连会让「以为走了代理」的部署直接暴露真实出口 IP
- `NewRequest` 的 `path` 是**拼接**到 BaseURL 上的（`joinPath`），所以沙箱 URL 必须转成相对路径（client.go:1258-1273，`sandboxPath`）—— 传绝对 URL 会拼出 `/https://host/...` 这种畸形路径，服务端只回 404，看起来像「端点不存在」
- `Warmup(ctx, n)`（client.go:211-256）：并发预建 n 条 TCP+TLS 连接，冷启动首个请求的 DNS+TCP+TLS 三轮往返（跨洋链路 300ms+）挪到启动阶段。**裸连接无法塞回 net/http 空闲池（没有公开 API）**，但完成一次握手已让 DNS/路由/TLS session ticket 全部就绪

**重试**（`Client.Do`, client.go:192-306）：
- 指数退避 + **±25% 抖动**（`backoff`，避免多实例齐步走）
- **重试风暴的廉价随机**：`fakeRand` 是内联 xorshift64，**避免为抖动引入 math/rand 的全局锁**（client.go:1702-1713）
- 尊重 `Retry-After`（数字秒或 HTTP date 两种格式，`parseRetryAfter`）
- `429`/`503` → 按 `Retry-After` 等待，重试耗尽后返回带 `RetryAfter` 的 `APIError`
- `5xx` 值得重试；**`4xx` 是确定性错误，重试只会浪费配额**
- **`bodyReader`（流式上传）无法重放，第一次之后就不能再试** —— 重试会发出空 body
- 重试时用 `req.GetBody` 重新读 body 而不必再序列化一遍

`IsNetworkError`（client.go:276-307）判定可安全重试的连接层错误：`net.Error`、`url.Error.Timeout()`，以及字符串兜底 `"connection reset by peer"` / `"connection refused"` / `"no such host"` / `"i/o timeout"` / `"EOF"` / `"http2: server sent GOAWAY"` / `"unexpected EOF"`（标准库没有导出的 sentinel）。

`drainClose`（client.go:1734-1740, pool.go:711-721）—— **必须排空再关闭**，否则这条连接不会被 net/http 放回空闲池，在高 QPS 下表现为连接数持续爬升、TIME_WAIT 溢出。

### 7.2 号池（`internal/account/pool.go` + `account.go`）

**无锁读热路径**：`accounts atomic.Pointer[[]*Account]` —— 热重载时整体替换切片，正在飞的请求继续用旧切片，不会看到半更新状态。

**调度策略**（`pick`, pool.go:262-327）：
```
round_robin | least_inflight（默认）| random | sticky_hash | weighted
```
默认 `least_inflight` 的评分公式：
```go
score := float64(a.inflight.Load())
if a.Weight > 0 { score /= float64(a.Weight) }   // 平票取权重高者
```
理由（pool.go:305-307）：**它天然把慢请求摊开**，避免 round_robin 把新请求继续压在已经拥堵的账号上。

**会话粘性**（pool.go:178-204）：`stickyKey` 非空优先复用之前绑定的账号 —— **Prism 的项目/会话是账号私有的，同一对话换号会直接 404，所以粘性不是优化而是正确性要求**。

`stickyMap`（pool.go:621-709）：
- **64 分片**（`shards [64]stickyShard`）：单个 RWMutex 在高 QPS 下会成为全局瓶颈，而这张表每次请求都要读一次
- FNV-1a 内联哈希（`idx`）避免额外函数调用
- TTL 30min + `janitor` 周期清理（防长期运行内存泄漏）

**冷却与自愈**（`MarkResult`, pool.go:382-406 + `Account.MarkFailure`, account.go:146-161）：
```go
case creds.IsAuthError(err):     // 401/403 → 标记 AuthFailed + 指数冷却
case creds.IsRateLimited(err):   // 429 → 用 Retry-After，缺省走指数冷却
case httpc.IsNetworkError(err):  // 仅 failures++
default:                          // 仅 failures++
```
**`authFailed` 标记是关键区分**（account.go:44-49 原文）：限流等一会儿就好，值得等；凭据失效等多久都没用，应该立刻报错让人去换凭据。少了这个标记，一个配错的 token 会让每个请求都空等一整个冷却周期。`allAuthFailed` 时直接报错而不是白等（pool.go:210-214）。

冷却退避：`Cooldown=60s`、`CooldownBackoff=2`、`MaxCooldown=30min`。

**全池冷却的行为**（pool.go:206-258）：
- 等最早解除冷却的那个，**加 20ms 抖动**（避免所有等待者同时醒来抢同一个账号）
- 但 `MaxWait`（默认 10s）上限：**一个 HTTP 请求挂几分钟等账号解冻是不可接受的**，客户端早就超时了，而调用方拿不到任何有用信息
- 并发上限全满时明确报出 `全部 N 个账号已达并发上限`（冷却是时间问题，并发上限则可能是容量不足 —— 后者值得明确报出来）

**刷新去重**（`RefreshAccount`, pool.go:484-522）：`refreshing atomic.Bool` + `CompareAndSwap`；已有刷新在飞时**用 `sync.Cond` 等待它结束然后直接读结果**。
理由（pool.go:481-483）：用 Mutex + 条件变量而不是 `x/sync/singleflight` —— 需要「等待者拿到刷新后的新凭据」这一语义，singleflight 只能共享结果，且会引入一个额外依赖。

**凭据刷新两条路径**（`creds.Refresher.Refresh`, refresh.go:264-304）：
```
refresh_token -> https://auth.openai.com/oauth/token   （最稳，可无限续期）
session cookie -> /api/auth/session                     （次之）
```
`refresh_task 被吊销（invalid_grant）时继续尝试 session 路径`（session cookie 往往还活着）。OAuth client_id = `app_jqKb52JverFFcl5GP4axT8QY`（**取自 Prism 自己 access token 里的 client_id 声明**；Codex CLI 用的是另一个 `app_EMoamEEZ73f0CkXaXp7hrann`，两者不通用 —— 拿错会得到「凭据明明有效却刷新失败」的假象，config.go:679-687）。轮换的 `refresh_token` 必须跟进。

**健康巡检**（pool.go:524-607）：只探「没有过期信息」或「已经过期」的账号（有明确未来 exp 的交给刷新循环处理，探了也是浪费配额）；探针走 `/api/auth/session` 而不是真实推理请求（后者会消耗额度且可能污染会话历史）。

### 7.3 凭据热重载（`internal/account/store.go`）

- **mtime + size 轮询**（5s 一轮，`Changed()`）而不是 fsnotify：跨平台行为差异（**尤其容器挂载卷的 inotify 事件不可靠**）会引入难以排查的「改了没生效」。一次 stat 在微秒级
- 文件不存在时返回 `(nil, nil)` —— **刻意设计：服务应当能先起来，凭据后补**
- `Persist` 原子写：`WriteFile(tmp, 0o600)` + `Rename`，写回后**同步内部状态**避免 Watcher 把自己写的当成外部变更再刷一遍
- 权限：`os.MkdirAll(dir, 0o700)`、`0o600` —— 这里存的是可以直接登录你账号的凭据
- 字段名容忍：`normKey` 归一化（小写 + 丢弃 `_`/`-`/空格），所以 `access_token`/`accessToken`/`Access-Token` 都能识别；同时接受 `{"accounts":[...]}` 与裸数组

### 7.4 SQLite 持久化（`internal/account/sqlite.go`）

- `modernc.org/sqlite`（**纯 Go，零 CGO**）
- `db.SetMaxOpenConns(1)`、`PRAGMA journal_mode=DELETE`（**确保 Windows 下句柄关闭后彻底释放文件**）、`PRAGMA synchronous=NORMAL`
- 5 张表：`accounts` / `request_logs` / `chat_sessions` / `chat_messages` / `api_keys`
- `upsert` 的 CASE 写法（sqlite.go:221-230）：空 token 不覆盖已有值
- 首次启动 `MigrateIfEmpty`：自动把文件里的账号迁移进 SQLite，「开箱即用无缝接管」
- 管理面：`/admin/accounts`(GET/POST/PUT/DELETE)、`/admin/accounts/{id}/refresh`、`/admin/reload`、`/admin/stats`、`/admin/requests`、`/admin/statistics`、`/admin/chat/sessions*`、`/admin/apikeys`、`/admin/login`、`/admin/me`、`/dashboard/`

### 7.5 指标（`internal/metrics/metrics.go` + `app.go`）

**零依赖实现**（源码理由：`prometheus/client_golang` 会带入 4~5 个间接依赖、注册全局状态，且每次 `WithLabelValues` 要做一次 map 查找加锁；我们只需要十几个指标）。

- `Counter` = `atomic.Int64`；`CounterVec` = `sync.Map`（key 为标签值拼接）
- **`key()` 用 `\x00` 而不是逗号分隔**（metrics.go:281-284）：标签值里出现逗号是常见情况（如 URL path），用逗号会产生键碰撞，把两个不同标签的序列合并，**指标静默失真**
- **Histogram 的累积语义**（metrics.go:70-92）：一次观测**只增加「它落进去的那个最小桶」**的计数，累积和是在 Render 时算的。若写成「对所有 `seconds <= b` 的桶都 +1」再在 Render 里累积，同一次观测会被重复计数，**分位数会系统性偏大 —— 这是一个很难靠肉眼发现的错误，因为指标看起来「有数」**
- `GaugeFunc`：账号在途数、goroutine 数这类指标**在请求路径上维护会污染热路径**，放到抓取时按需计算
- `Render` 不叫 `WriteTo`（那个签名会被 go vet 误判为想实现 `io.WriterTo`）
- 内存：只在抓取时读 `runtime.ReadMemStats`（它会 STW）

指标清单（`oaiprism_*`）：`http_requests_total{path,method,code}`、`http_request_duration_seconds`、`upstream_requests_total{path,code}`、`upstream_request_duration_seconds`、`upstream_retries_total`、`facade_runs_total{api,model,status}`、`facade_latency_seconds`、**`facade_first_delta_seconds`（首字延迟，流式体验的核心指标）**、`sse_deltas_total`、`tokens_total{api,direction}`、`poll_rounds_total{result}`、`poll_empty_total`、`project_ops_total`、`sandbox_ops_total`、`conversation_ops_total`、`account_pick_total`、`capture_records_total`、`client_aborts_total`、`panics_total`、`auth_failed_total`、`rate_limited_total`

**中间件链**（`internal/middleware/middleware.go:1-8`）：`Recover -> RequestID -> Metrics -> RateLimit -> Auth -> handler`
- Recover 必须在最外层，否则一次 panic 会带走整个连接且没有任何日志
- Metrics 在 Auth 之前，这样**鉴权失败也能被观测到**
- `statusRecorder` **必须实现 Flusher**（middleware.go:164-167）：否则包一层就会让所有流式响应的 Flush 失效 —— 这是包装 ResponseWriter 最经典的坑。另实现 `Unwrap()` 让 `http.ResponseController` 能找到底层 writer
- `routeLabel` 路径归一化（**直接用原始 path 会让指标基数爆炸，每个 UUID 一条时间序列，最终把 Prometheus 打挂 —— 这是监控里最常见的自伤方式之一**）
- API Key 用 `subtle.ConstantTimeCompare` 且**不 break**（让比较次数与 key 数量固定，不泄漏「命中了第几个」）
- 探针端点 `/healthz`/`/readyz`/`/metrics` 豁免鉴权（**k8s/Docker 探针拿 401 会导致容器被反复重启**）
- CORS 的 `Access-Control-Expose-Headers: x-prism-conversation-id, x-request-id` 是关键（middleware.go:100-102）：自定义响应头不在 CORS 安全列表里，浏览器默认不给 JS 读，少了它跨域客户端拿不到会话 ID，**多轮续写直接断链 —— 而且不报错，只是「每次都是新会话」**

### 7.6 抓包（`internal/capture/capture.go`）

**有界 channel + 丢包**：队列满了就丢弃并计数，而不是阻塞请求。**宁可丢几条日志，也不能让生产流量变慢**。
- 单写盘协程（`loop`），按天切分 `capture-{YYYY-MM-DD}.jsonl`，权限 `0o600`
- 敏感头打码：`authorization`/`cookie`/`set-cookie`/`x-api-key`/`api-key`/`proxy-authorization`
- 确定性采样（递增序号取模，避免随机数开销）
- `Summarize(records)` **把真实抓包变成「协议字段表」**（按 method+normalized path 汇总所有出现过的 JSON key + 状态码）—— 你据此填 YAML 里的 `schema` 段，就不用靠猜了
- `ReplayFile` 供离线校准与单测

---

## 8. rawproxy 白名单原样反代

`internal/rawproxy/handler.go` —— 「完全不懂协议也能用」的保底通道。

### 8.1 挂载与路径改写

```go
prefix := strings.TrimRight(h.cfg.RawProxy.Prefix, "/")  // 默认 /prism
if prefix == "" { prefix = "/prism" }
mux.Handle(prefix+"/", h)   // 以 "/" 结尾的模式匹配整棵子树
mux.Handle(prefix, h)
```
理由（handler.go:68-69）：**上游端点众多且随时会新增，逐个登记不现实**。

```go
upstreamPath := strings.TrimPrefix(r.URL.Path, prefix)   // /prism/api/llm/xxx -> /api/llm/xxx
if r.URL.RawQuery != "" { upstreamPath += "?" + r.URL.RawQuery }
```

### 8.2 路径白名单（`allowed`, handler.go:342-360）

```go
func (h *Handler) allowed(path string) bool {
	list := h.cfg.RawProxy.AllowPaths
	if len(list) == 0 { return true }          // 空 = 全放开（不推荐）
	p := path
	if i := strings.IndexByte(p, '?'); i >= 0 { p = p[:i] }   // 查白名单前先剥 query
	for _, a := range list {
		if a == "" { continue }
		if strings.HasPrefix(p, a) { return true }
	}
	return false
}
```

默认白名单（config.go:537-548）：
```yaml
allow_paths:
  - /api/projects
  - /api/project-access
  - /api/codex/conversation-history
  - /api/llm/
  - /api/lim/          # 保留笔误路径（有客户端照着旧文档写）
  - /api/backend/
  - /api/y
  - /s/sandboxes/
  - /api/project-files/
  - /api/auth/session
```
拒绝时返回 403 且错误信息**直接告知怎么放开**（`如需放开请修改 raw_proxy.allow_paths`）。

### 8.3 鉴权与安全边界

**双重丢弃**（handler.go:143-158 出站、181-196 入站）：

```go
// 出站：客户端的认证信息一律丢弃，换成池里账号的
if lk == "authorization" || lk == "cookie" { continue }
h.setCredentials(hdr, cred)

// 入站：绝不把上游的 Set-Cookie 透给调用方
if lk == "set-cookie" { continue }
```
出站丢弃是**安全边界**：否则调用方可以用自己的 cookie 覆盖账号池。
入站丢弃的理由（handler.go:188-189 原文）：那是账号池里账号的会话凭据，**一旦泄漏等于把账号送人**。

`setCredentials`（handler.go:311-339）：注入 `Cookie`/`Authorization`，并补浏览器化请求头（`Origin`/`Referer`/`User-Agent`/`Accept-Language`），最后**账号级 headers 无条件覆盖**。

**hop-by-hop 头过滤**（handler.go:74-90）：
```go
var hopByHop = map[string]struct{}{
	"connection": {}, "proxy-connection": {}, "keep-alive": {},
	"proxy-authenticate": {}, "proxy-authorization": {},
	"te": {}, "trailer": {}, "transfer-encoding": {}, "upgrade": {},
	"content-length": {},   // 由我们按目标 body 重新设置
	"host": {},             // Host 由 http.Client 依据 URL 设置
}
```

**账号选取**（`acquire`, handler.go:297-309）：先看 `X-Oaiprism-Account` 显式指定，否则用 `X-Oaiprism-Session` 做粘性键。`PassthroughAuth`（默认 false）才允许客户端自带凭据。

### 8.4 流式搬运（`copyBody`, handler.go:219-260）

```go
streaming := resp.ContentLength < 0 ||
	strings.Contains(resp.Header.Get("Content-Type"), "event-stream")
if !streaming {
	n, err := io.CopyBuffer(w, resp.Body, buf)   // 已知长度：交给 net/http 自己缓冲
	return n, err
}
// 流式：逐块 Read + Write + Flush
fl, canFlush := w.(http.Flusher)
for { n, err := resp.Body.Read(buf); ... if canFlush { fl.Flush() } ... }
```
缓冲从 `sync.Pool` 取（`BufferSize` 默认 64KB）。

`metricPath`（handler.go:421-440）：路径里的 UUID（36 字符 4 个 `-`，或任意 ≥24 字符段）替换成 `{id}` —— **原样反代的路径带 UUID（`/api/projects/{uuid}/thumbnail`），直接当标签会让时间序列数量随项目数线性增长，最终拖垮 Prometheus**。

---

## 9. 值得抄的 5 个设计 + 要避开的 3 个坑（带文件:行号）

### ✅ 值得抄的 5 个设计

**1. 协议漂移三件套：宽容解析 + 配置化字段名 + 抓包回放**
`internal/prism/extract.go:1-14`、`client.go:707-737`、`config.go:307-356 / 563-633`、`capture.go:350-410`
- 「精确优先、宽容兜底」的解析顺序是**有语义理由的**：本协议用 HTTP 200 + 嵌套 error 表达失败，只有精确解析能可靠区分「成功但还没输出」与「已经失败了」（client.go:704-706）
- 字段名一律从 YAML 读（`FieldInput`/`FieldTurnState`/`RespIDKeys`…），协议变了改配置不改代码
- 所有原始报文可被 `capture` 录制，`Summarize()` 直接把抓包变成「协议字段表」

**2. 沙箱工作区同步的「两层缓存 + 令牌过期即失效点 + 按项目并发锁」**
`internal/facade/sandbox.go:21-49 / 84-97 / 138-145`、`runner.go:665-753`
- 沙箱按 (账号) 缓存、同步状态按 (账号,项目) 缓存 —— **粒度差异是有原因的**（资源令牌 JWT 里编码了 project_uuid）
- **用资源令牌的 `Expiry()` 当缓存失效点，而不是自己拍 TTL**：令牌过期后沙箱必然读不到项目资源，与其等失败再重试，不如到点主动重同步
- `LockProject` 保证并发相同请求**等**前一个同步完成（各自去签一份资源令牌会同时开多个沙箱会话，白耗额度）
- 关键洞察：**不需要在 Go 里实现 Yjs/lib0** —— 把凭证交给沙箱，沙箱自己连 Y-Sweet（实测结论）

**3. 把轮询协议伪装成真流的 `Diff` 前缀差分 + SSE 写出行 + 15s 心跳**
`internal/prism/extract.go:245-277`、`internal/sse/writer.go`、`responses.go:150-180 / 426`
- `Diff(prev, cur) -> (delta, reset)`：不管上游返回累计全文还是结构化消息列表，只要单调增长就能还原 token 级增量
- `sync.Pool` 复用的 8KB 缓冲 + 手写 JSON 编码器（不做 HTML 转义）+ 预编码常量帧 → 稳态零分配
- `X-Accel-Buffering: no` + 每帧 `Flush()` + 15s 心跳事件：三者缺一，「流式」要么被 Nginx 缓冲成一次性返回，要么被中间链路按空闲掐断
- **`response.failed` 必须是标准终止事件**（encode.go:317-325）：只发自定义 `error` 时 Codex CLI 状态机等不到任何终止事件，报 `stream closed before response.completed`，真正的失败原因全丢

**4. 号池的「粘性是正确性要求」+ authFailed 与 rateLimited 的语义区分**
`internal/account/pool.go:178-204 / 382-406`、`account.go:44-49 / 77-85 / 146-161`
- `stickyKey` 不是优化：**Prism 的项目/会话是账号私有的，同一对话换号会直接 404**
- 64 分片 stickyMap（FNV-1a 内联）+ `atomic.Pointer[[]*Account]` 无锁读 + 热重载整体换切片
- `authFailed` 标记把「限流（值得等）」与「凭据失效（等多久都没用）」分开 —— 少了它，一个配错的 token 会让每个请求空等一整个冷却周期
- `least_inflight` 默认策略天然把慢请求摊开；`MaxWait` 上限避免请求挂在注定超时的等待上

**5. 工具桥：用提示词协议把「云端沙箱大脑」接到「本地 CLI 手脚」**
`internal/facade/toolbridge.go:43-76 / 168-174 / 234-250`、`responses.go:182-263`、`toolcall.go:131-165`
- 显式抑制上游自带沙箱工具 + **末尾重申指令**（实测模型会被 CLI 传入的 Codex 人设压过开头指令）
- `ensureExecJS` **代理层兜底**：模型没按 JS 格式输出 shell 命令时自动包上 `exec_command`，避免「语法错误→模型困惑→换个姿势再错」的死循环
- `bridgeRetryNudge` 只自动纠正**一轮**（打破模型「我在云端执行成功了」的错觉）
- `[CLIENT RESULT call_id=... tool=...]` 明确标注，让模型分得清「工具回传的结果」与「用户提到的一个名字」
- 工具调用条目（`custom_tool_call`/`function_call`）**必须原样回放为文本**，否则每轮都会重新规划已经做过的操作

### ❌ 要避开的 3 个坑

**1. 「HTTP 200 就是成功」的假设 —— 本协议失败也是 200**
`internal/prism/client.go:282-296`、`client.go:775-793 / 913-934`、`runner.go:576-590`
```
失败形态：HTTP 200 + status:"completed" + response.status:"error"
          + response.payload.reason / message / rootCause
```
只看状态码的实现会把失败当成功，返回一个「成功的空回答」。必须**优先走强类型包络解析**（`PrismEnvelope`），宽容解析对「错误」的识别是启发式的。
同理还有：**`turn_state` 必须原样回传**（自造值被拒成 `turn_state is required`）、**每轮必须更新 turn_state**（忘了就永远拿到同一个 pending，表现为「永远不结束」，runner.go:515-516）。

**2. 漏掉「沙箱 + 工作区同步」后，故障现象全指向错误的地方**
`internal/prism/types.go:29-46`、`runner.go:301-328`、`client.go:1337-1344`
- 不带 `sandbox_url`/`sandbox_token` → `reason:"sandbox_reconnecting"`，**看起来像「上游挂了」**
- 只申请沙箱、不注入资源令牌 + Y-Sweet 凭证 → **不报错，只是永远停在 `syncing`，最终固定 122 秒后 504**，看起来像「冷启动慢」
- 上游 `codexRequestDebug` 里其实写着 `sandbox_url_resolved: null`，但没人会去看
- 沙箱端点认证是**双重**的（Cookie + `X-Crixet-Sandbox-Token`），只带后者**直接 401 且响应体为空** —— 极难排查（client.go:1443-1447）
- 未就绪时 `wait-for-sync` **会直接断 TLS（EOF）**，这不是错误，只是「还没好」，网络错误必须继续等（client.go:1284-1285）
- 重试窗口要按上游分钟级限流来设：**CLI 会自动重连约 12 分钟（5×2m23s），服务端只重试 15 秒就放弃 → 两边窗口严重错配**（runner.go:613-621）

**3. 热路径上的几个「看着没事、实际在自伤」的写法**
- **轮询无条件节流**（runner.go:559-574）：「有增量就立刻再问一次」会让循环退化成零间隔忙轮询，4 次轮询 20ms 跑完 = 对上游做拒绝服务。这个 bug 是被另一个测试顺带抓出来的。
- **指标标签不做归一化**（middleware.go:228-231 / rawproxy handler.go:421-440 / metrics.go:281-284）：原始 path 当标签 → 每个 UUID 一条时间序列 → Prometheus 被打挂；标签值用逗号分隔 → 键碰撞 → 指标静默失真。
- **Histogram 累积语义写错**（metrics.go:70-92）：一次观测对多个桶 +1 再在 Render 累积 = 重复计数，分位数系统性偏大，**指标看起来「有数」所以极难发现**。
- **包装 ResponseWriter 忘了实现 Flusher**（middleware.go:164-167）：包一层就让所有 SSE 的 `Flush()` 失效，流式退化成攒 4KB 才发。
- **`drainClose` 缺失**（httpc/client.go:1734-1740）：不排空 body 就 Close，连接不会被放回空闲池，高 QPS 下连接数持续爬升 + TIME_WAIT 溢出。
- **`http.Client.Timeout` 设了值**（client.go:93-95）：全局 Timeout 会把正常的长回答误杀；流式与长轮询应交给 context。
- **`DisableCompression: false`**（client.go:81 + 注释 9-11）：Go 偷偷加 gzip 并自动解压，让「原样透传」的 `Content-Encoding`/`Content-Length` 全部对不上。
- **`CheckRedirect` 默认跟随**（client.go:96-100）：上游 302 意味着登录态失效，跟过去只会拿到 HTML 登录页，反而掩盖真实错误。
- **上游 401 原样透传成 401**（chat.go:341-361）：会让客户端以为自己的 API Key 有问题并反复重试；对调用方这是「网关的上游凭据挂了」，应映射成 **502**。

### 🕳️ 本仓库的 2 个明显缺口（重写时应补上）

1. **`cached_tokens` 完全未实现**（§5.4）：上游 usage 只有 3 个字段，OpenAI 的 `prompt_tokens_details.cached_tokens`、Anthropic 的 `cache_read_input_tokens` / `cache_creation_input_tokens` 一律被丢弃。可抄改进：按会话前缀稳定性自算并要求上游回传。
2. **`tools` 字段处理仍是「待验证」状态**（translate.go:260-266、config.go:331-333）：`toolsMetadata` 塞进 `metadata`，源码注释明确写着「拿到真实凭据后应当用一次抓包确认工具该放哪里」；`FieldUserID` 也是推断值。重写时应先用 capture 模式定论。

---

## 可复用文件清单

| 文件 | 行数 | 可复用要点 | 复用度 |
|---|---|---|---|
| `internal/prism/types.go` | 598 | 全部端点常量 + 强类型包络（`PrismEnvelope`/`CodexPayload`/`CodexOutputItem`/`CodexDeltaFile`）+ `SandboxSyncStatus.Ready()` 前端同款判定 + `ResourceToken.Expiry()` 取更早者 | ★★★★★ 直接抄 |
| `internal/prism/client.go` | 1781 | 浏览器化请求头、`Do` 的重试/退避/抖动/Retry-After/一次性 body、`sandboxPath` 相对路径、双重认证头、Y-Sweet Raw 原样转发、`extractCodexOutput` 倒序取最后一条 | ★★★★★ 直接抄 |
| `internal/prism/extract.go` | 315 | `Diff(prev,cur)->(delta,reset)` 把轮询转流式、BFS `FindKey`、`FindLongestString`、`FlattenContent`（含 `[]map[string]any` 分支）、`DecodeAny`+`UseNumber()` | ★★★★★ 直接抄 |
| `internal/facade/runner.go` | 920 | start+poll 编排、`syncSandboxWorkspace` 四步、`isSandboxNotReady` 判定表、无条件节流、`stopUpstream` 独立 5s context、换号重试边界（已吐内容不重试）、`isAccountLevel` | ★★★★★ 直接抄 |
| `internal/facade/toolbridge.go` | 290 | 桥 prompt 全文、末尾重申、`extractExecBlock`、`ensureExecJS` 兜底、`bridgeRetryNudge` 单轮纠正、`customToolCallItemJSON`、`SetEscapeHTML(false)` | ★★★★★ 直接抄 |
| `internal/facade/sandbox.go` | 181 | 两层缓存粒度、令牌过期当失效点、同 token 保留 projects、`LockProject`、`Invalidate` vs `InvalidateProject` | ★★★★★ 直接抄 |
| `internal/sse/writer.go` | 330 | `sync.Pool` 缓冲、预编码帧、并发安全写、`X-Accel-Buffering`、手写 `AppendJSONString`/`AppendInt`/`AppendFloat` | ★★★★★ 直接抄 |
| `internal/httpc/client.go` | 350 | Transport 全部调参（`MaxIdleConnsPerHost=256`、`DisableCompression=true`、32KB 缓冲、`ForceAttemptHTTP2`）、`CheckRedirect` 不跟随、`Warmup`、`IsNetworkError`、`RetryableStatus`、socks5 明确报错 | ★★★★★ 直接抄 |
| `internal/facade/encode.go` | 357 | 手写 chunk 编码（Chat / Anthropic / Responses 三套事件）+ `response.failed` 终止事件 | ★★★★☆ 直接抄 |
| `internal/facade/chat.go` | 393 | 三协议流式/非流式骨架、`estimateTokens`、`mapError`（401→502、cancel→499）、`passthroughFields` 未知字段直通 | ★★★★☆ 直接抄 |
| `internal/facade/http.go` | 442 | 会话键推导（header → user → system+首条 user 指纹）、metadata 保留键过滤、`conversationIDFrom`/`previousResponseIDFrom` 三来源、编译期断言 64 字符表 | ★★★★☆ 直接抄 |
| `internal/facade/responses.go` | 426 | Responses 事件全序列、15s 心跳 goroutine、桥模式缓冲后再输出、`extractSentinelToken` | ★★★★☆ 参考 |
| `internal/facade/translate.go` | 370 | messages → input 数组、system 保 role、历史塞进 system（绕上游丢弃历史）、`imageURLAndDetail` 三形态、tool→`[name result]` 标注 | ★★★★☆ 参考 |
| `internal/facade/compress.go` | 102 | 确定性滑动窗口 + 结构化摘要（非 LLM）、单条 200 字符截断 | ★★★★☆ 直接抄 |
| `internal/facade/project.go` | 162 | 按 key 串行 + 引用计数的 `keyedLocker`、项目缓存按 (账号,会话) 键、轮转分桶、gc goroutine | ★★★★☆ 直接抄 |
| `internal/facade/toolcall.go` | 187 | `ExtractContentFromDiff` 还原文件、DeltaFiles→标准 ToolCalls 工具名探测、`ApplyLocalWorkspaceFiles` 路径遍历防护 | ★★★★☆ 直接抄 |
| `internal/facade/image.go` | 140 | 图片转存为 `input_file`、base64/外部 URL 双通道、排除内部域名、10MB/15s 限制 | ★★★★☆ 直接抄 |
| `internal/facade/anthropic.go` | 257 | Anthropic 事件序列、`message_start` 占位 + `message_delta` 给准确 output_tokens、`metadata.user_id` 作会话键 | ★★★☆☆ 参考 |
| `internal/facade/sandbox.go`+`journal.go` | 181+114 | 挂起日志（不自动重放）、`MarkTerminal` | ★★★☆☆ 参考 |
| `internal/account/pool.go` | 721 | `atomic.Pointer` 号池热重载、5 种调度策略、64 分片 stickyMap、`RefreshAccount` 用 Cond 去重、`MarkResult` 语义区分、`MaxWait` 上限 | ★★★★★ 直接抄 |
| `internal/account/account.go` | 279 | 全 atomic 热点字段、`authFailed` 标记、指数冷却 `MarkFailure`、`rate.Limiter` 可选 | ★★★★★ 直接抄 |
| `internal/account/store.go` | 456 | mtime+size 轮询热重载、`normKey` 字段名容错、原子 Persist（0600/0700）、文件不存在也能启动 | ★★★★☆ 直接抄 |
| `internal/account/sqlite.go` | 777 | 纯 Go SQLite、`MaxOpenConns(1)`、`journal_mode=DELETE`（Windows 释放句柄）、5 张表、`MigrateIfEmpty`、`request_logs` 用量字段 | ★★★★☆ 参考 |
| `internal/creds/creds.go` | 469 | `prism_*` cookie 集、`EffectiveCookie` 拼装、`applyJWT` 不验签就地取 exp/email/account_id/plan、`MergeCookie` 回写 Set-Cookie | ★★★★★ 直接抄 |
| `internal/creds/refresh.go` | 358 | 两条自愈路径（OAuth 优先，invalid_grant 回退 session）、refresh_token 轮换跟进、`APIError` 带 `RetryAfter` | ★★★★★ 直接抄 |
| `internal/capture/capture.go` | 432 | 有界 channel 丢包不阻塞、按天 jsonl、敏感头打码、确定性采样、`Summarize` 生成协议字段表、`ReplayFile` | ★★★★★ 直接抄 |
| `internal/rawproxy/handler.go` | 440 | 子树挂载、白名单前缀匹配、出站丢 cookie/authorization + 入站丢 set-cookie、hop-by-hop 过滤、流式搬运、`metricPath` UUID 归一化 | ★★★★★ 直接抄 |
| `internal/metrics/metrics.go` | 356 | 零依赖 Prometheus 文本格式、`\x00` 标签分隔、Histogram 单桶计数、`GaugeFunc` 抓取时求值 | ★★★★★ 直接抄 |
| `internal/metrics/app.go` | 116 | 指标清单集中于一处，避免「同一含义两个名字」的漂移 | ★★★★★ 直接抄 |
| `internal/middleware/middleware.go` | 473 | 中间件顺序理由、`statusRecorder` 实现 Flusher+Unwrap、`routeLabel` 低基数、恒定时间 API Key 比较、探针豁免、CORS `Expose-Headers` | ★★★★★ 直接抄 |
| `internal/config/config.go` | 1107 | 全部可调项 + `defaultSchema()` 已校准字段映射 + 模型映射表（含下架模型兼容重定向） | ★★★★☆ 参考 |
| `docs/Prism完整调用链.md` / `docs/协议校准报告.md` | — | 沙箱 + Y-Sweet 同步瓶颈实测、`/api/llm/` vs `/api/lim/` 纠错、122s 504 复现 | ★★★★★ 直接读 |

> 说明：「直接抄」= 逻辑与调参可直接移植（含中文注释里的实测结论）；「参考」= 结构可借鉴但需按新架构调整（如 `journal.go` 的挂起日志是否与 SQLite 持久化合并）。

---

## 附：一句话总结

`oai-prism` 的核心价值不在「能跑通 Prism」，而在于它把**每一个反直觉的协议事实都变成了带原因的代码与注释**：

1. 失败是 **HTTP 200 + `response.status:"error"`**，不是 4xx/5xx
2. `turn_state` 是**不透明续令牌，必须逐轮原样回传**
3. 沙箱 + 工作区同步（资源令牌 + Y-Sweet 凭证）**四步全做完才能 syncing→synced**，漏一步就是固定 122 秒 504
4. 上游**没有真流**，用 `Diff` 前缀差分 + SSE `Flush` + 15s 心跳合成
5. 上游**没有客户端 function calling**，工具靠「提示词围栏 + 代理解析 + 回灌」或「DeltaFiles → 标准 ToolCalls」两条桥
6. 号池**粘性是正确性要求**（会话/项目账号私有）
