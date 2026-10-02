# 03 · prismctl2api (Danchuna) 与 pm-plu (spacex-3) 逆向研究

> 目标：为「整合百家之长」的 Go 反代重写提炼最小可用骨架。
> 两份仓库都很小（2.6K / 3.2K 行 Go），但把 prism.openai.com 私有后端
> （`/api/llm/response_with_tools_start|_status` + `/s/sandboxes/proxy/heartbeat`）
> 的字段级细节钉得很死，是最有参考价值的两块拼图。
>
> 代码基线：2026-09/10 版本。所有结论都直接来自源文件，未做推测性补全。

---

## 0. 两仓库定位对比

| 维度 | prismctl2api | pm-plu |
|---|---|---|
| 形态 | 独立 HTTP 网关（`net/http` 直连上游） | CLIProxyAPI（CPA）**C 动态库插件** |
| 入口 | `main.go` → 监听 `127.0.0.1:8899` | `main.go` 是 cgo `//export` 插件符号 |
| 对外协议 | OpenAI Chat / Anthropic Messages / Responses / models | CPA `executor.execute(_stream)`（Responses 格式） |
| 上游 HTTP | 自己 `http.Client`（HTTP/1.1 强制） | **经宿主 `host.http.do`** 代理发出 |
| 凭据 | `bootstrap.json` + `cookie.txt` | `type: prism` 的 auth 文件（`session.json`） |
| 沙箱 | 复用 bootstrap 里那**一个**热沙箱 + `conversationId` | **沙箱池**，可 `POST /api/backend/1/new` 铸新并保温 |
| 工具桥接 | `-local-tools` 开关，网关翻 `tool_calls` | 默认就翻转，把模型当 next-action emitter |
| 流式 | 真 SSE 增量（轮询期间即时吐 toolCalls） | **伪流式**：完成后合成整段 SSE |

**一句话分工**：prismctl2api 提供「怎么把上游 start/status 讲对、怎么吐三种协议、怎么做客户端工具桥接」；pm-plu 提供「怎么铸沙箱做池、怎么保温、怎么回退模型、怎么把 Codex 巨型上下文折叠成一条消息」。

---

# A. prismctl2api（Danchuna/prismctl2api）

文件：`main.go`(444) `gateway.go`(1024) `gateway_openai.go`(391) `gateway_anthropic.go`(222) `gateway_test.go`(167) `grab_token.py` `extract_prompt.py` `local_agent.py`。

## A.1 上游 URL / header / body（字段级）

### 常量（`main.go:35-41`）

```go
const (
    baseURL    = "https://prism.openai.com"
    startPath  = "/api/llm/response_with_tools_start"
    statusPath = "/api/llm/response_with_tools_status"
    hbPath     = "/s/sandboxes/proxy/heartbeat"
    userAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36"
)
```

### 三条链路

| 调用 | 方法与路径 | 关键头/体 |
|---|---|---|
| 心跳/预热 | `GET hbPath + "?prism_cache_bust=" + <unixMilli>` | 头 `x-crixet-sandbox-token: <sandbox_token>`；body 为 `nil` → `req.Method` 置 GET |
| 发起回合 | `POST startPath` | `input[]` + `metadata{}` + `conversationId`（可选 `previousResponseId`） |
| 轮询状态 | `POST statusPath` | body 仅 `{"request_id":…, "turn_state":…}` |

### 统一请求头（`main.go:115-136` `upstreamCtx`）

```go
h.Set("accept", "*/*")
h.Set("accept-language", "zh-CN,zh;q=0.9")
h.Set("origin", baseURL)
h.Set("referer", baseURL+"/")
h.Set("priority", "u=1, i")
h.Set("sec-ch-ua", `"Not=A?Brand";v="99", "Google Chrome";v="151", "Chromium";v="151"`)
h.Set("sec-ch-ua-mobile", "?0")
h.Set("sec-ch-ua-platform", `"Windows"`)
h.Set("sec-fetch-dest", "empty")
h.Set("sec-fetch-mode", "cors")
h.Set("sec-fetch-site", "same-origin")
h.Set("user-agent", userAgent)
if body != nil { h.Set("content-type", "application/json") }
if cookie != "" { h.Set("cookie", cookie) }
for k, v := range extra { h.Set(k, v) }   // extra 用于塞 x-crixet-sandbox-token
```

> 注意 `sec-ch-ua-platform: "Windows"` 与 UA 的 Windows 段必须自洽，因为 `cf_clearance`
> 绑定「出口 IP + UA」，见 README 坑位 5。

### start 请求体（`main.go:230-265` 与 `gateway.go:407-453` 两处同构）

```go
meta := map[string]any{
    "projectId":        pick(f.ProjectID, bootstrap.ProjectID),
    "userId":           pick(f.UserID, bootstrap.UserID),
    "model":            pick(f.Model, "gpt-6-astra"),
    "reasoning_effort": pick(f.ReasoningEffort, "xhigh"),
    "frontend_origin":  baseURL,
    "sandbox_url":      sandboxURL,          // 缺省 baseURL+"/s/sandboxes/proxy/"
}
if sandboxToken != "" { meta["sandbox_token"] = sandboxToken }              // 关键
if snap := pick(f.ListenSnapshot, bootstrap.ListenSnapshot); snap != "" {   // 关键
    meta["codex_listen_snapshot"] = json.RawMessage(snap)
}
body := map[string]any{
  "input": []any{
     {"type":"message","role":"system","content":[]any{{"type":"input_text","text":ctx}}},
     {"type":"message","role":"user",  "content":[]any{{"type":"input_text","text":f.Prompt}}},
  },
  "metadata": meta,
}
if conv != "" { body["conversationId"] = conv }
if f.PreviousResponseID != "" { body["previousResponseId"] = f.PreviousResponseID }
```

**硬约束（README 坑位 2、3）**：
- 缺 `sandbox_token` → HTTP **200** 但 `payload.reason="sandbox_reconnecting"`、`codexRequestDebug.sandbox_token_present=false`，永远等不到答案。
- `listen_snapshot` / `sandbox_token` 绑定到某个会话，`conversationId` 必须配套，否则上游当新会话处理 → 同样 `sandbox_reconnecting`。
- `status` 不把 `turn_state` **原样**回传 → **401**。

### 上游响应结构（`gateway.go:211-283`）

```go
type upstreamToolCall struct {
    LineIndex        int    `json:"line_index"`
    CallID           string `json:"call_id"`
    Name             string `json:"name"`
    CallType         string `json:"call_type"`
    ArgumentsPreview string `json:"arguments_preview"`
}
type upstreamResponse struct {
    Status  string `json:"status"`
    ID      string `json:"id"`
    Payload *struct {
        Reason  string `json:"reason"`
        Message string `json:"message"`
        Output  []struct {                        // 答案在 message/assistant/content[].text
            Type, Role string
            Content []struct{ Type, Text string } `json:"content"`
        } `json:"output"`
        CodexDeltaFiles []struct{ FilePath, Status string } `json:"codexDeltaFiles"`
        CodexRequestDebug *struct {               // 沙箱侧真实失败原因
            SandboxTokenPresent, ListenSnapshotPresent bool
            Error *struct{ Status int; StatusText, URL, BodyText string } `json:"error"`
        } `json:"codexRequestDebug"`
    } `json:"payload"`
}
type upstreamState struct {
    Status         string            `json:"status"`
    RequestID      string            `json:"request_id"`
    ConversationID string            `json:"conversation_id"`
    TurnState      json.RawMessage   `json:"turn_state"`      // 原样回传
    Response       *upstreamResponse `json:"response"`
    CodexLive      *struct {
        LineCount int                `json:"lineCount"`
        ToolCalls []upstreamToolCall `json:"toolCalls"`
    } `json:"codex_live_progress"`
}
```

`text()` 只取 `payload.output[]` 中 `type=="message" && role=="assistant"` 的 `content[].text`，多段用 `\n\n` 拼。
`failure()` 优先读 `payload.codexRequestDebug.error.bodyText`（再套一层 `{"error":{"message":…}}`），否则退回 `payload.reason` —— 于是能拿到
`504 Timed out waiting for sandbox workspace file synchronization` 这种真实原因。

## A.2 凭据：bootstrap.json / cookie.txt / grab_token.py

`bootstrap.json` 结构（`main.go:85-93`）：

```go
var bootstrap struct {
    Cookie         string `json:"cookie"`
    SandboxToken   string `json:"sandbox_token"`
    SandboxURL     string `json:"sandbox_url"`
    ConversationID string `json:"conversation_id"`
    ProjectID      string `json:"project_id"`
    UserID         string `json:"user_id"`
    ListenSnapshot string `json:"listen_snapshot"`   // JSON 字符串（对象序列化后再存）
}
```

- `cookie.txt`：纯文本整条 Cookie 串；`readTextFile` 会**剥掉 UTF-8 BOM**（`main.go:149-156`，因为 Windows 记事本/PowerShell `-Encoding UTF8` 会写 BOM 导致 `json.Unmarshal` 报错）。
- Cookie 必须含 `oai-sc`、`prism_session_token`、`prism_oai_access_token`、`cf_clearance`、`__cf_bm`；少 `oai-sc` → `401 Could not parse your authentication token`。
- 时效：`prism_session_token` ~12h；`sandbox_token` 实测 ≥1.5h，沙箱回收即失效。

`grab_token.py`（从 HAR 抠字段，输出 bootstrap.json）：

```python
md = start_req.get("metadata", {})
snap = as_obj(md.get("codex_listen_snapshot"))    # metadata 里是「JSON 字符串」，转回对象
conv = start_req.get("conversationId") or (snap or {}).get("conversation_id")
bs = {
    "sandbox_token": md.get("sandbox_token", ""),
    "sandbox_url":   md.get("sandbox_url", "https://prism.openai.com/s/sandboxes/proxy/"),
    "project_id":    md.get("projectId", (snap or {}).get("project_id", "")),
    "user_id":       md.get("userId",    (snap or {}).get("user_id", "")),
    "conversation_id": conv or "",
    "listen_snapshot": json.dumps(snap, ensure_ascii=False) if snap else "",
    "cookie": "",      # HAR 不含 cookie，需另填
}
```

- 定位 entry：`e["request"]["url"].endswith("response_with_tools_start")`，`json.loads(e["request"]["postData"]["text"])`。
- `extract_prompt.py`：同法取 `input[]` 里 role==system 且 `len(text) > 500` 的那条（上下文 JSON 那条较短，用长度区分），写出 `prism_system_prompt.txt`。

## A.3 回合循环：turn_state 原样回传 + toolCalls 只在 pending 累积

`runTurn`（`gateway.go:390-542`）核心：

```go
code, raw, err := upstreamRetry(ctx, startPath, body, cookie, nil, 2) // 只有传输失败才重试
...
if st.Status == "completed" { emitAnswer(tr, st.Response, emit); return nil } // start 可能直接完成
rid, ts := st.RequestID, st.TurnState
seen := map[string]bool{}
tick := time.NewTicker(pollInterval)             // 默认 1200ms
for {
    select { case <-ctx.Done(): return ctx.Err(); case <-tick.C: }
    if time.Now().After(deadline) { emit(error,"回合超时未完成"); return nil }
    _, praw, perr := upstreamRetry(ctx, statusPath,
        map[string]any{"request_id": rid, "turn_state": ts}, cookie, nil, 3) // 只读轮询多试
    var ps upstreamState
    json.Unmarshal([]byte(praw), &ps)
    if len(ps.TurnState) > 0 { ts = ps.TurnState }   // 原样回传，游标推进
    if ps.CodexLive != nil {                          // 只在 pending 期间非空
        for _, tc := range ps.CodexLive.ToolCalls {
            key := tc.CallID
            if key == "" { key = fmt.Sprint("line:", tc.LineIndex) }
            if seen[key] { continue }
            seen[key] = true
            emit(turnEvent{Kind:"tool", ID:key, Name:pick(tc.Name,"tool"), Args:tc.ArgumentsPreview})
        }
    }
    switch ps.Status {
    case "completed": emitAnswer(tr, ps.Response, emit); return nil
    case "failed","error": emit(error, ps.Response.Payload.Reason); return nil
    }
}
```

关键点：
- **`turn_state` 每次轮询都从响应里取新的（`ts = ps.TurnState`）原样回传**，绝不自己构造 → 否则 401。
- `codex_live_progress.toolCalls` / `eventPreviews` **只在 pending 期间回传，completed 时为空**，必须边轮询边累积（轮询 1.2s）；去重键优先 `call_id`，退化到 `"line:"+line_index`。
- 这些是**沙箱自己已经执行完**的工具调用，只是流给客户端看过程；不要拿去再执行（README 明示）。
- 超时 `turnTimeout` 默认 8 分钟；单次 start 重试 2 次、status 3 次。

## A.4 `-local-tools` 客户端工具桥接（完整协议 + prompt 原文）

**上游事实**（README 实测表）：网页端 start 请求体顶层只有 `input`/`metadata`/`conversationId`，**没有 `tools`**；手动塞 `tools` 会被忽略（模型回 `no_tool_available`）；模型用的 `exec_command` 等全是服务端 Codex 内置工具。→ **上游没有客户端 function-calling 通道**，桥接是用「模型输出 JSON 下单」绕过。

### 协议 prompt（`gateway.go:560-579`，原文摘录）

```
## 客户端工具代理模式（必须遵守）
你的沙箱工具不适用于本次任务。需要执行动作时，你只能输出一个 JSON 对象来表达要调用的客户端工具，
不要解释、不要用命令行、不要读写沙箱文件：

{"tool_calls":[{"name":"<工具名>","arguments":{...}}]}

规则：
- 一次可以给多个条目，调用方会按顺序执行。
- 调用方会把执行结果以 [tool_result ...]（或 role=tool 的消息）放进后续对话里，你看到结果后继续。
- 本轮如果不需要工具就能回答，就直接用纯文本回答，不要输出 JSON。
- 工具名必须来自下面列出的清单，参数必须符合其 schema。

## 在 Windows 上执行命令的注意事项（能省好几轮）
- 调用方多半是 Codex，exec_command 在 Windows 上跑的是 **PowerShell**，不是 bash：
  不要用 ls / cat / [ -f x ] / && 这类写法，改用 Get-ChildItem / Get-Content / Test-Path。
- **不要把补丁文本或 heredoc 嵌进 PowerShell 命令**：apply_patch 会因引号转义失败，
  报 The first line of the patch must be *** Begin Patch。写文件请直接：
  Set-Content -LiteralPath '文件' -Value '内容' -Encoding UTF8
```

无工具时的环境说明（`gateway.go:553-558` `noLocalToolsNotice`）—— 防止模型在沙箱里做完后谎报"已在你电脑上创建"：

```
## 环境说明（重要）
本次请求的客户端**没有声明任何本机工具**，因此你**无法访问用户的电脑**。
- 你只能在自己的沙箱工作区里读写文件；那与用户的电脑无关。
- **绝对不要**声称"已在你的电脑/桌面/磁盘上创建或修改了文件"。
```

### 系统文本拼装（`bridgeSystemText`, `gateway.go:799-809`）

```go
func (tr turnRequest) bridgeSystemText() string {
    var parts []string
    if s := strings.TrimSpace(tr.System); s != "" { parts = append(parts, "## 调用方系统指令\n"+s) }
    parts = append(parts, toolProxyProtocol+renderToolsForProxy(tr.Tools))
    if len(tr.History) > 0 {
        parts = append(parts, "## 本次请求之前的对话（含工具执行结果）\n"+renderHistory(tr.History))
    }
    return strings.Join(parts, "\n\n")
}
```

桥接模式**不注入** Prism 人格提示词（否则"用你自己的工具"与"只用 JSON 下单"打架），并把 `reasoning_effort` 固定为 `low`（`gateway.go:403-406`）。

### 模型清单缓存（`gateway.go:73-123`）

Codex 桌面端 `tools: null`（走代码模式），缓存 CLI 报过的清单（`tools_cache.json`）顶上：
`effectiveTools(declared)` → 有声明就缓存并用；无声明就 `loadCachedTools()` 兜底。

### 宽容解析（`gateway.go:601-797` + `gateway_test.go`，18 个用例）

```go
var (
    fenceRe     = regexp.MustCompile("(?s)```[a-zA-Z_]*\\s*(\\{.*?\\})\\s*```")
    commaTailRe = regexp.MustCompile(`,\s*([}\]])`)
    nameStrip   = strings.NewReplacer("-","", "_","", " ","", "\t","")
)
func normName(s string) string { return strings.ToLower(nameStrip.Replace(strings.TrimSpace(s))) }

func parseToolCalls(text string, tools []toolDef) []parsedCall {
    // 1) 建立 allowed[归一化名]=真名
    // 2) 弯引号/加粗/反引号清洗
    cleaned := strings.NewReplacer("“","\"","”","\"","‘","'","’","'","**","","`","").Replace(text)
    // 3) 候选来源：```代码块（含语言标记） → 清洗后文本的花括号平衡片段 → 原文的平衡片段
    // 4) decodeToolCalls 逐个尝试
}
```

`jsonCandidates` 用「花括号平衡 + 跳过字符串内括号」扫描；`decodeToolCalls` 兼容：
- 容器键 `tool_calls` / `calls` / 单对象 `action` / 裸单对象；
- 名字键 `name`/`tool_name`/`tool`/`function`（function 可为对象或字符串）；参数键 `arguments`/`args`/`input`/`parameters`；
- 尾逗号 `commaTailRe`、`arguments` 双重编码 `normalizeArgs`（首字符为 `"` 时解开一层）。
- **未声明工具名也照样返回**（只记日志），不静默漏成纯文本。

### 回灌（`emitAnswer`, `gateway.go:812-831`）

```go
if localTools {
    if calls := parseToolCalls(text, tr.Tools); len(calls) > 0 {
        for _, c := range calls { emit(turnEvent{Kind:"tool", ID:newID("call"), Name:c.Name, Args:c.Args}) }
        return
    }
}
emit(turnEvent{Kind:"text", Text:text})
```

**注意：不要求 `len(tr.Tools) > 0`** —— 客户端漏传 tools 时模型仍可能按旧协议输出 JSON，也必须转成 tool_call。

### 各协议 finish 语义

| 协议 | 工具调用事件 | finish/stop |
|---|---|---|
| Chat Completions | `choices[].delta.tool_calls[]`（`index`/`id`/`type:"function"`/`function.name`/`function.arguments`） | 桥接且 n>0 → `finish_reason:"tool_calls"`，否则 `"stop"`，末帧 `data: [DONE]` |
| Anthropic | `content_block_start`(tool_use) → `content_block_delta`(`input_json_delta`,`partial_json`) → `content_block_stop` | 桥接 → `stop_reason:"tool_use"` |
| Responses | `response.output_item.added`(function_call) → `response.function_call_arguments.delta` → `response.output_item.done` | 无（completed 里同时带 function_call 与 message item） |

Responses 侧还有个关键：`parseResponsesInput` 必须认 `function_call` / `function_call_output`，否则 Codex 回灌的执行结果被丢成空消息，模型看不到输出会**反复重下同一条命令**（`gateway_openai.go:206-244`）。

### `local_agent.py`（本机 agent 参考实现）

- 声明 6 个本机工具：`list_dir`/`read_file`/`write_file`(overwrite|append)/`make_dir`/`delete_file`/`run_shell`。
- `safe_path` 把路径锁在 `--root` 内（`os.path.commonpath` 防穿越）。
- `shell_policy`：`--allow-shell-write` 关时只放行只读命令头白名单（`READ_OK_HEADS` / `PIPE_OK_HEADS`），`FORBIDDEN` 无条件拒绝。
- 循环：POST `/v1/chat/completions`（带 `tools`）→ 拿 `tool_calls` → 本机执行 → `{"role":"tool","tool_call_id":…,"content":"[tool_result]\n"+out}` 回灌 → 最多 `--max-iters`（默认 12）。

## A.5 conversationId 复用与沙箱连续会话

- `pick(tr.ConversationID, bootstrap.ConversationID)`：默认复用 bootstrap 里那一个 `conversationId`，于是同沙箱会话（cwd、文件）多轮连续（`gateway.go:451-453`、README「请求翻译规则」）。
- 因此 `sandbox_token`/`listen_snapshot`/`conversationId` 三者是配套的，缺一不可。

## A.6 采样参数被忽略 + usage 按字符估算

- `temperature`/`max_tokens`/`top_p` 等**被静默忽略**（沙箱不接受）；`max_tokens` 只读入 `anthRequest` 但不使用。
- **`usageEstimate`**（`gateway.go:1022-1024`）：

```go
func usageEstimate(prompt, completion string) (int, int) {
    return len(prompt)/4 + 1, len(completion)/4 + 1
}
```

Chat 侧还先把 prompt 折成 `strings.Repeat("x", usageIn)` 再估（`gateway_openai.go:125`），只保证量级。

## A.7 HTTP/1.1 强制 + Cloudflare PROTOCOL_ERROR 重试

```go
// TLSClientConfig 非 nil 且不强制 h2 → 走 HTTP/1.1。
// Cloudflare 上 Go 的 http2 连接复用会偶发 "connection error: PROTOCOL_ERROR"。
var httpc = &http.Client{Transport: &http.Transport{
    ResponseHeaderTimeout: 300 * time.Second,
    IdleConnTimeout:       60 * time.Second,
    TLSClientConfig:       &tls.Config{},
}}
```

重试策略（`main.go:52-69`）：只对**传输层错误**（RST/GOAWAY/EOF）重试，退避 `250*(i+1) ms`；`start`（非幂等、会创建回合）只 `attempts=2`，`status`（只读）`attempts=3`。
（Go 里 `TLSClientConfig` 非 nil 且未显式 `ForceAttemptHTTP2` 时不会自动升 h2，即锁定 HTTP/1.1。）

---

# B. pm-plu（spacex-3/pm-plu）

文件：`prism.go`(635) `translate.go`(579) `execute.go`(261) `main.go`(346) `models.go`(198) `jsonutil.go`(185) `auth.go`(137) `config.go`(127) `stream.go`(100) `hostcall.go`(46)。形态是 CLIProxyAPI（CPA）**C ABI 插件**（cgo）。

## B.1 铸沙箱 `POST /api/backend/1/new` 与「token 必须是热的」

上游路径常量（`prism.go:11-19`）：

```go
const (
    prismNewPath    = "/api/backend/1/new"
    prismStartPath  = "/api/llm/response_with_tools_start"
    prismStatusPath = "/api/llm/response_with_tools_status"
    maxPoolSize      = 60
    defaultKeepalive = 600
    pollInterval     = 1500 * time.Millisecond
)
```

铸造（`prism.go:227-241`）：

```go
func mintSandbox(m attemptMeta) (poolEntry, error) {
    status, decoded, err := prismPost(m, prismNewPath, map[string]any{}, mintTimeoutFor())
    ...
    token := stringField(decoded, "token")   // 响应 {token, url}
    url   := stringField(decoded, "url")
    if token == "" || url == "" { return poolEntry{}, fmt.Errorf("sandbox mint response had no token/url") }
    return poolEntry{URL: normalizeSandboxURL(url), Token: token}, nil
}
```

`normalizeSandboxURL` 保证结尾带 `/`。

**「token 必须是热的」约束（README 工作方式第 4 条）**：新铸的沙箱要冷启动几分钟然后 504，**凭证里的 `sandbox_token` 是从真实浏览器请求中截取的、指向已在运行的沙箱**。所以：
- 池**首发**用凭证导入的那个 seed 沙箱（`p.seed`，恰好一次性发放，`seeded` 标志避免两个请求共用一个）；
- 之后才按需 `mintSandbox` 扩容（`acquire` 仅在 `time.Until(deadline) > 10s` 时才发起这个 multi-second 冷铸）；
- 空闲沙箱靠 `keepaliveLoop` 保温，避免冷启动 504。

### 沙箱池（`prism.go:83-298`）

- 每个 cookie（账号）一个池：`pools map[string]*sandboxPool`，所有沙箱共享该 cookie。
- `acquire(deadline)`：先非阻塞取 idle；→ 未发放的 seed；→ 有预算时铸新；→ 否则阻塞到 deadline。
- `release(e, dead)`：`dead` 则 `size--` 丢弃；否则塞回 idle 队列，队列满也 `size--`。
- `pingSandbox`：对 idle 跑一次 `prismPass(… "ping", model, "low", …)`，`passOK→True`、`passDead→False`（丢弃），auth/model 问题不算沙箱问题（保留）。
- 一个沙箱一次只处理一个 turn，多发会被上游 400；`concurrency` 上限 60（对齐 Pro 账号）。

### 一次 pass（`prismPass`, `prism.go:413-498`）

```go
meta := map[string]any{
    "model": model, "reasoning_effort": effort,
    "frontend_origin": strings.TrimRight(m.BaseURL, "/"),
    "sandbox_url":     entry.URL,
    "sandbox_token":   entry.Token,
}
if m.ProjectID != "" { meta["projectId"] = m.ProjectID }
if m.UserID    != "" { meta["userId"]    = m.UserID }
// start → 200 且 request_id 非空才算成功；否则 400/401/403 → passAuth（重试无益），其余 passTransient
// start 可能直接终局：status ∈ {completed,error,failed} 或缺 turn_state → classifyTerminal
reqState := map[string]any{"request_id": decoded["request_id"], "turn_state": decoded["turn_state"]}
for time.Until(deadline) > 0 {         // 轮询：turn_state 原样续回
    _, d, _ := prismPost(m, prismStatusPath, reqState, pollTimeout)
    if ts, ok := d["turn_state"]; ok && ts != nil { reqState["turn_state"] = ts }
    switch stringField(d, "status") { case "completed","error","failed": return classifyTerminal(d,"") }
    time.Sleep(pollInterval)           // 1500ms
}
```

> 注意 pm-plu **没有**在轮询期流出 toolCalls —— 它把沙箱工具当成干扰，只取最终文本。

### 终局分类（`classifyTerminal`, `prism.go:500-527`）

```go
if stringField(rs,"status") == "success" { return passOK{text: responseOutputTextFrom(rs)} }
reason := stringField(pay, "reason")
why := truncate(reason+": "+message+" "+codexRequestDebug.error.bodyText, 400)
if unsupportedReason(body) { return passUnsupported }              // 含 "unsupported assistant model"
if reason == "sandbox_reconnecting" || numberField(pay,"httpStatus") == 504 {
    return passDead                                                 // 冷/死沙箱 → 丢弃重铸
}
return passTransient
```

`passKind`：`passOK / passAuth / passUnsupported / passDead / passTransient / passFailed / passTimeout`。

## B.2 模型白名单与 400 Unsupported assistant model 自动回退

`models.go:23-49`（注释：*Prism 的 allowlist 会无预警变化，2026-09-17 白天把 `gpt-6-astra` 下架*）：

```go
var prismModels = []prismModel{
    {PublicID:"gpt-6-astra",   UpstreamID:"gpt-6-astra",   Display:"GPT 6 Astra", Context:272000},
    {PublicID:"gpt-5.6-sol",   UpstreamID:"gpt-5.6-sol",   Display:"5.6 Sol",     Context:272000},
    {PublicID:"gpt-5.6-terra", UpstreamID:"gpt-5.6-terra", Display:"5.6 Terra",   Context:272000},
    {PublicID:"prism-astra",   UpstreamID:"gpt-6-astra",   …},  // 别名
    {PublicID:"prism-sol",     UpstreamID:"gpt-5.6-sol",   …},
    {PublicID:"prism-terra",   UpstreamID:"gpt-5.6-terra", …},
}
var prismFallbacks = map[string][]string{
    "gpt-6-astra":   {"gpt-5.6-sol","gpt-5.6-terra"},
    "gpt-5.6-sol":   {"gpt-5.6-terra","gpt-6-astra"},
    "gpt-5.6-terra": {"gpt-5.6-sol","gpt-6-astra"},
}
```

回退驱动（`prism.go:317-347` `runPrismTurn`）：

```go
model := substituteFor(wantedModel)          // 进程内粘性替换
text, why := turnWithModel(m, pool, system, user, model, effort, turnDeadline)
if text != "" || why == "" { return text, nil }
if unsupportedReason(why) {                  // 400: Unsupported assistant model
    for _, alt := range fallbacksFor(wantedModel) {
        if alt == model { continue }
        text, why = turnWithModel(m, pool, system, user, alt, effort, turnDeadline)
        if text != "" { rememberSubstitute(wantedModel, alt); return text, nil }
        if !unsupportedReason(why) { break }
    }
}
return "", fmt.Errorf("%s. Re-import a fresh Prism session (auth file) and retry", why)
```

- `reasoningEffort` 只接受 `low/medium/high`（`reasoningEfforts`），别名表把 `xhigh/x-high/max/ultra→high`、`minimal/none→low`（`models.go:34-43`）—— **与 prismctl2api 的 5 档不同，pm-plu 归一成 3 档**。
- 模型名后缀：`gpt-5.6-sol(high)`、`gpt-5.6-sol:high`（仅当后缀是已知 effort 才剥离），支持 `prism/` 前缀、`model_aliases`（yaml，多跳 ≤4，防环）。
- 单轮三次 start+poll 内自愈：`passDead→release(dead)+睡 2s`、`passTransient→release(false)+睡 3s`、`passAuth/Unsupported/Failed/Timeout→立刻返回原因`（`turnWithModel`, `prism.go:352-401`）。**一个 budget 覆盖「排队 + 整轮」**。

## B.3 折叠成一条 user + next-action emitter（prompt 原文与解析）

### 上游事实（README 工作方式）

1. **Prism 只保留最后一条 user 消息** → 整段对话必须折进那一条。
2. 模型自带沙箱，直接让它"做任务"会在**它自己的沙箱**里做 → 改成只输出一个 JSON 动作，插件再解析成标准 `function_call` 交客户端执行。
3. 丢弃 Codex 注入的超长 developer 指令，只保留 shell 环境（cwd）。

### 协议 prompt（`translate.go:15-55`，原文摘录）

```
<role>next-action emitter</role>

You are ONE COMPONENT IN A PIPELINE. A separate executor process runs commands on
the user's machine. You do not. You never execute anything, you never touch a
filesystem, and you never report work as done from your own knowledge.

Your entire contract: read the task plus the transcript of what the executor has
already run, then emit ONE JSON action. The executor runs it and appends the result
to the transcript, then asks you again. Prose output breaks the pipeline and is
discarded, so never explain, never apologise, never use markdown fences.

Available actions:
%s

Shell guidance:
- `apply_patch` is NOT a shell command. Never pipe into it, never call it from a shell.
- Do NOT use heredocs (<<EOF). The executor's shell often cannot create the temp file they need.
- To write a file, redirect printf:
    printf '%s\n' 'first line' 'second line' > path/to/file
- To read a file use `cat`, to search use `rg`. Verify with `cat` after writing.
- A command failing does not mean the workspace is read-only...
- File paths in arguments are paths on the EXECUTOR's machine... never pass a path you have not seen in the transcript.
- When a schema says two parameters are mutually exclusive, send only one.

Emit exactly one of:
  {"tool_call":{"name":"<action>","arguments":{...}}}
  {"done":"<one line summary>"}     <- ONLY when the TRANSCRIPT already proves the task
                                       is complete. An empty transcript never proves anything.
```

末尾再钉一条 reminder（`toolReminder`，每个 user 消息尾部）：

```
================================================================================
Emit ONE JSON action now. Actions: %s
{"tool_call":{"name":"...","arguments":{...}}}  or  {"done":"..."}
Nothing else. Start your reply with { .
================================================================================
```

### 组装（`assemble`, `translate.go:241-290`）

```go
catalog := … fmt.Sprintf("- %s\n    params: %s\n    %s", t.Name, truncateRunes(mustCompact(t.Parameters),700), t.Description)
system := fmt.Sprintf(toolProtocol, strings.Join(catalog, "\n"))
if ctx := envContext(envParts); ctx != "" {
    system += "\n\nThe executor runs here. Use these real paths - never invent a " +
        "sandbox path like /codex_workspace/...:\n" + ctx
}
// 取最后一条 [user] 作为 TASK，其余作为 transcript（clampTranscript 保最近的、超预算截尾）
user := fmt.Sprintf("TASK:\n%s\n\nTRANSCRIPT SO FAR:\n%s", orNone(task), transcript) +
        fmt.Sprintf(toolReminder, strings.Join(names, ", "))
return conversation{System: system, User: user}
```

- 最终只产出 **1 条 system + 1 条 user**（上游只认最后一条 user）。
- `clampTranscript`：从最新往回装，超 `max_transcript_chars`（默认 24000）截断，并在头部插 `...(N earlier steps omitted)...`。
- 无工具时退化为「system 拼 sysParts、user 拼 convo」，若有多条 `[user]` 则前缀 `Conversation so far. Respond to the FINAL message.`。

### 环境提取（`envContext`, `translate.go:59-67/171-205`）

```go
envBlockRe = regexp.MustCompile(`(?s)<environment_context>.*?</environment_context>`)
envTagRe   = regexp.MustCompile(`(?s)<cwd>.*?</cwd>|(?s)<filesystem>.*?</filesystem>`)
cwdLineRe  = regexp.MustCompile(`(?im)^.{0,40}(cwd|current working directory)\s*[:=].*$`)
noiseRe    = regexp.MustCompile(`^<(recommended_plugins|plugin_instructions|skills_instructions|apps_instructions)\b`)
```

- 从 developer/instructions/system 文本里只抠 `<environment_context>` / `<cwd>` / `<filesystem>`，去重后限 1500 runes；抠不到再退回正则找 cwd 行（≤3 条）。
- `flattenResponses` 里 `role=="user"` 且文本以 `noiseRe` 开头（Codex 脚手架：plugins/skills/apps 指令）**直接丢弃** —— 这条正是「丢弃超长 developer 指令」的实现。

### 各类 item → transcript 标记（`flattenResponses`, `translate.go:309-376`）

| Responses item | 折叠成 |
|---|---|
| `function_call` | `[executor ran]\n<name> <arguments>` |
| `function_call_output` | `[result]\n<output>`（截 8000 runes） |
| `custom_tool_call` | `[executor ran]\n<input>`（截 4000） |
| `custom_tool_call_output` | `[result]\n<output>` |
| `reasoning` / `additional_tools` / `item_reference` | 丢弃 |
| role `developer`/`system` | sysParts + envParts |
| role `assistant` | `[assistant]\n<text>` |
| 其它 | `[user]\n<text>` |

`flattenChat` 对 `role=="tool"` → `[tool result]\n…`；assistant 带 `tool_calls` → `[executor ran]\n<compact(calls)>`。

### 工具清单内联（`clientToolEntries`, `translate.go:86-162`）

- 跳过 `type=="web_search"`、无名、以及 `namespace` 以 `mcp__` 开头的（*MCP surface is too big to inline*）。
- `function`：取 description 第一段、限 300 runes，参数 schema 来自 `parameters`/`input_schema`/`inputSchema`；`custom`：freeform 无 schema，描述限 160 runes。
- `tool_choice == "none"` → 返回 nil（不注入任何工具）。
- 也扫 `input[]` 里 `type=="additional_tools"` 的嵌套 tools。

### 解析（`translate.go:435-530`）

```go
func stripFence(text string) string {          // 去 ``` 围栏；若首个 { 在前 40 字符内则从它开始
    s := strings.TrimSpace(text)
    if strings.HasPrefix(s, "```") { s = regexp.MustCompile("^```[a-zA-Z]*\n?").ReplaceAllString(s,""); s = strings.TrimSpace(regexp.MustCompile("```$").ReplaceAllString(s,"")) }
    if i := strings.Index(s, "{"); i > 0 && i < 40 { s = s[i:] }
    return s
}
func loadsJSON(s string) (map[string]any, bool) {   // 容忍模型丢掉尾部花括号：补 0~3 个 } 再试
    if !strings.HasPrefix(s, "{") { return nil, false }
    for extra := 0; extra <= 3; extra++ {
        var v any
        if err := decodeJSONUseNumber([]byte(s+strings.Repeat("}", extra)), &v); err == nil {
            if o, ok := v.(map[string]any); ok { return o, true }
            return nil, false
        }
    }
    // 再容忍「有效对象后跟散文」：Decoder 只解第一个值
    dec := json.NewDecoder(strings.NewReader(s)); dec.UseNumber()
    var v any
    if err := dec.Decode(&v); err == nil { if o, ok := v.(map[string]any); ok { return o, true } }
    return nil, false
}
func parseToolCall(text string) *parsedToolCall {    // object["tool_call"] -> {name, arguments|input}
    // arguments 可为对象或「JSON 字符串」（解开一层再 compact）；custom 用 input 字符串
    // 统一 ID = "call_" + randomHex(8)
}
func parseDone(text string) (string, bool) { … object["done"].(string) }
```

`parsedToolCall{ID,Name,Arguments,Input,Custom}`；`execute.go:162-201` `responseItem` 把它转成 Responses item：

```go
if call.Custom {
    item := {"type":"custom_tool_call","id":"ctc_"+call.ID,"call_id":call.ID,"name":name,"input":call.Input,"status":"completed"}
    if entry.Namespace != "" && entry.Namespace != "functions" { item["namespace"] = entry.Namespace }
    return item, mustCompact(call)
}
item := {"type":"function_call","id":functionItemID(call.ID),"call_id":call.ID,"name":name,"arguments":call.Arguments,"status":"completed"}
```

- `lookupTool` 先精确匹配，再按最后一个 `.` 取后缀匹配（处理 `namespace.tool` 写法）。
- 若 `parseToolCall` 为空但有 `{"done":…}` → 取 done 文本当普通 assistant message。
- `functionItemID` 限长 64，超长时 `fc_+sha256(call_id)[:61]`。

## B.4 `data/session.json` 凭据字段与 import_session.py

`session.json`（README_CN「凭证」）：

```json
{
  "type": "prism",
  "cookie": "__Secure-next-auth.session-token=...; 其他cookie",
  "sandbox_url": "https://xxx.prism.openai.com/",
  "sandbox_token": "从请求体 metadata 拷贝",
  "project_id": "可选",
  "user_id": "可选"
}
```

代码侧 `auth.go:53-82` `parseCredential`：

```go
acceptedAuthTypes = map[string]bool{"prism":true,"free-astra":true,"freeastra":true,"prism2api":true}
cred := prismCredential{
    Type:         providerID,                                        // "prism"
    Cookie:       firstString(payload, "cookie", "cookies"),
    SandboxURL:   firstString(payload, "sandbox_url", "sandboxUrl"),
    SandboxToken: firstString(payload, "sandbox_token", "sandboxToken"),
    ProjectID:    firstString(payload, "project_id", "projectId"),
    UserID:       firstString(payload, "user_id", "userId"),
    raw:          payload,
}
if cred.Cookie == ""       { return …, fmt.Errorf("prism auth requires cookie") }
if cred.SandboxToken == "" { return …, fmt.Errorf("prism auth requires sandbox_token …") }
cred.NextRefreshAfter = time.Now().Add(6 * time.Hour).UTC()
```

`authRecord`（写回 CPA 的 `~/.cliproxy/auths/`）：`Provider`/`ID`(= `"prism-"+sha256(cookie)[:8]`)/`FileName`/`Label`(=`"Prism project "+projectID`)/`StorageJSON`(紧凑 JSON：type/cookie/sandbox_url/sandbox_token[/project_id][/user_id])/`Metadata`/`Attributes`/`NextRefreshAfter`。
**没有 refresh token**：`auth.refresh` 只是重新 `parseCredential` + `record`，靠用户重导 cURL 覆盖文件，CPA 下个刷新周期自动重读，无需重启。Cookie 约 12h 过期。

生成方式（README_CN）：登录 `prism.openai.com` → DevTools Network 发一条消息 → 右键 `/api/llm/response_with_tools_start` → **Copy as cURL** → `python3 scripts/import_session.py prism-curl.txt -o ~/.cliproxy/auths/prism.json`（再手动把 `"type"` 改成 `"prism"`）。
> ⚠️ **该仓库内不含 `scripts/import_session.py`**（只有 `scripts/package.py`），README 指向「主 README」。其行为只能是：从 cURL 里解出 `-H 'cookie: …'` 与 `--data-raw` 的 JSON，取 `metadata.sandbox_token`/`sandbox_url`/`projectId`/`userId`，写上述 5~6 字段 —— 与 prismctl2api 的 `grab_token.py` 等价，只是输入从 HAR 变成 cURL。

## B.5 CPA 插件 ABI 与流式实现

### ABI（`main.go:3-115`）

```c
typedef struct { uint32_t abi_version; void* host_ctx;
                 cliproxy_host_call_fn call; cliproxy_host_free_fn free_buffer; } cliproxy_host_api;
typedef struct { uint32_t abi_version;
                 cliproxy_plugin_call_fn call; cliproxy_plugin_free_fn free_buffer;
                 cliproxy_plugin_shutdown_fn shutdown; } cliproxy_plugin_api;
```

导出符号：`cliproxy_plugin_init` / `cliproxyPluginCall` / `cliproxyPluginFree` / `cliproxyPluginShutdown`；
`const abiVersion uint32 = 1; const schemaVersion uint32 = 6`。
`envelope{OK bool; Result json.RawMessage; Error *{Code,Message,HTTPStatus}}` 是所有方法统一返回。

方法分派（`main.go:127-162`）：`plugin.register`/`plugin.reconfigure`（→`configure` + 注册信息）、`plugin.quiesce`/`plugin.shutdown`、`model.register`/`model.static`/`model.for_auth`、`auth.identifier`/`executor.identifier`、`auth.parse`、`auth.login.start`/`poll`、`auth.refresh`、`executor.execute`/`execute_stream`、`executor.count_tokens`。
`pluginRegistration` 里 `capabilities`：`model_registrar/model_provider/auth_provider/executor=true`，`executor_input_formats=["openai-response","codex"]`。

### 宿主调用（`hostcall.go`）

```go
const (
    hostHTTPDo, hostHTTPDoStream, hostHTTPStreamRead, hostHTTPStreamClose = "host.http.do", …
    hostStreamEmit = "host.stream.emit"; hostStreamClose = "host.stream.close"; hostLog = "host.log"
)
```

**关键架构差异**：pm-plu 的 **上游 HTTP 请求不是自己发出的**，而是包成 `{method,url,headers,body,wire_profile}` 交给宿主 `host.http.do`；`prismPost`（`prism.go:574-635`）里

```go
"wire_profile": map[string]any{"disable_auto_compression": true}
```

—— 由宿主保证不自动解压缩（对齐浏览器行为）。超时用 `time.After` + goroutine 竞速实现。

### 流式（`stream.go` + `execute.go:105-157`）

> 注释原文：*Prism has no token stream: the finished answer arrives from the poll loop.*
> 所以 pm-plu 是**伪流式**：真结果等 poll 完成后，再合成整段 Responses SSE。

`startPrismStream`：
1. 校验 `stream_id`；立即 `emitPluginStream(stream_id, encodeSSE("response.created", …))` 与 `response.in_progress` —— 唯一的实质好处是客户端马上进「thinking」，不挂住。
2. 起 goroutine 跑 `runPrismTurn`（带 `defer recover()` → `closePluginStream(id, "stream panic: …")`）。
3. 成功后构造 `item, measured := responseItem(text, tools)`、`base["usage"] = usageObject(...)`。
4. `synthesizeStreamFrames(base, item, text, isTool)` 一次性吐完帧序列，再 `closePluginStream(id, "")`。

`synthesizeStreamFrames`（`stream.go:46-87`，`streamTextDeltaSize = 600`，对齐 freeastra.py）：
`response.output_item.added` → 工具调用的 `response.function_call_arguments.delta/done`（或文本 `response.output_text.delta`×N → `.done`）→ `response.output_item.done` → `response.completed` → `data: [DONE]`。

### usage 估算（`translate.go:545-577`）

```go
func estimateTokens(value any) int {   // 拉丁词按 (len+3)/4，CJK/其它每 token 计 1
    count := 0
    for _, piece := range tokenRe.FindAllString(text, -1) {
        if latinRe.MatchString(piece) { count += (len(piece)+3)/4; if count == 0 { count = 1 } } else { count++ }
    }
    return count
}
func estimatedUsage(system, user, output string) (int, int) {
    input  := estimateTokens(system) + estimateTokens(user) + 4
    outputT := estimateTokens(output) + 1
    ...
}
```

`tokenRe` 把 CJK（`\x{4e00}-\x{9fff}` 等 4 段）单列，`cjkRe` 保留作「若 Prism 将来上报真实 usage 则做更细 CJK 加权」。`count_tokens` 方法则简单 `len(payload)/4`。usage 对象含 `input_tokens/output_tokens/total_tokens` + `input_tokens_details.cached_tokens=0` + `output_tokens_details.reasoning_tokens=0`。

### 配置（`config.go`）

`pluginConfig`（yaml）：`enabled`(默认 true) / `base_url`(默认 `https://prism.openai.com`) / `user_agent`(默认 macOS Chrome/152) / `effort`(默认 `medium`) / `timeout_seconds`(默认 240) / `concurrency`(默认 1，上限 60) / `keepalive_seconds`(默认 600) / `max_transcript_chars`(默认 24000) / `model_aliases`(map)。
用 `atomic.Value` 存 config，`configure` 时 Store 并 `startKeepalive()`。

### 测试文件（可作回归夹具）

`pool_test.go`(225) / `main_test.go`(313) / `testhelpers_test.go`(12)：`doHost` 是变量可被测试替换（`var doHost = callHost`），池行为、翻译、解析都有覆盖。

---

## C. 可复用文件清单

| 目标能力 | prismctl2api 抄哪段 | pm-plu 抄哪段 | 建议 |
|---|---|---|---|
| 上游 HTTP 客户端（HTTP/1.1 + 重试） | `main.go:43-69`（`httpc` + `upstreamRetry`） | `prism.go:574-635`（`prismPost`，走宿主） | 独立网关抄前者；插件形态抄后者 |
| start 请求体组装（meta 字段） | `gateway.go:407-453` | `prism.go:413-447` | **两者合看**，字段完全一致 |
| turn_state 原样回传 / 轮询 | `gateway.go:485-541` | `prism.go:466-497` | 抄 prismctl2api（多一段 toolCalls 累积） |
| 沙箱铸造 + 池 + 保温 | ❌ 无（只复用热沙箱） | `prism.go:83-312` 全部 | **只此一家，必抄** |
| 模型白名单 + 400 自动回退 | `gateway.go:904-936`（仅白名单/后缀） | `models.go` 全文 + `prism.go:317-347` | **抄 pm-plu** |
| 把对话折成一条 user / 丢弃 Codex 噪声 | 无（历史进 system） | `translate.go` 全文 | **抄 pm-plu**（上游只认最后一条 user） |
| next-action emitter prompt | `gateway.go:560-579`（toolProxyProtocol） | `translate.go:15-55`（toolProtocol+reminder） | 取 pm-plu 主模板 + prismctl2api 的 Windows 注记 |
| 模型 JSON 输出宽容解析 | `gateway.go:601-797` + `gateway_test.go` | `translate.go:435-530` | **两者互补**：前者覆盖键名/围栏/弯引号/双编码，后者补「缺尾花括号」 |
| 工具清单缓存（桌面端 tools:null） | `gateway.go:73-123` | 无 | 抄 prismctl2api |
| 三种协议 SSE 适配 | `gateway_openai.go` `/` `gateway_anthropic.go` 全文 | `stream.go`（只 Responses 伪流） | **抄 prismctl2api** |
| Responses input 解析（function_call_output） | `gateway_openai.go:206-244` | `translate.go:309-376` | 两者都关键（防重复下命令） |
| 凭据提取脚本 | `grab_token.py`（HAR） | `scripts/import_session.py`（cURL，**仓库内缺失**） | 抄 `grab_token.py`；cURL 版自行补 |
| 本机工具 agent 参考 | `local_agent.py` 全文（6 工具 + 权限策略） | 无（工具由客户端执行） | 抄 prismctl2api |
| usage 估算 | `gateway.go:1022-1024`（`len/4+1`） | `translate.go:545-577`（拉丁/CJK 分权） | **抄 pm-plu**，更准 |
| 配置/热重载 | flag（启动期固定） | `config.go`（`atomic.Value` 热更） | 抄 pm-plu |

## D. 结论一句话

**这两份能拼出一个最小可用网关的「上游对话协议层 + 客户端工具桥接层」**：prismctl2api 给你「start/status/heartbeat 三链路讲对、turn_state 原样回传、三种协议 SSE、宽容解析、本机 agent 参考」，pm-plu 给你「沙箱池与热 token 保温、模型白名单自动回退、把 Codex 巨型上下文折叠成一条 user 的 next-action emitter、拉丁/CJK 分权 usage、插件式热配置」——把 prismctl2api 的协议适配壳套在 pm-plu 的上游会话/池/折叠管线上，就是骨架。
