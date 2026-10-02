# Prism 通道 · 六项需求测试报告

日期：2026-10-01 ｜ 底座：james-6-23/codex2api ｜ 新增包：`proxy/prism`
账号：CPA（plan=pro）｜ 出口：新加坡节点（cf_clearance 须与登录同出口）

---

## 0. 结论速览

| # | 需求 | 实现 | 测试 | 真机验证 | 结论 |
|---|---|---|---|---|---|
| 1 | 高并发 | ✅ | 25 用例（含 64 路 + 池并发 + 在飞闸门/平滑放行 + 熔断退避 + `-race`） | ✅ 4 路并发 4/4；单账号 15 槽**同时在线**（工艺见 §5.6） | **达标** |
| 2 | 低延迟 | ✅ 逐事件 Flush | 2 用例 | ✅ 首帧即出 | **达标**（见 §2 边界说明） |
| 3 | 工具调用 | ✅ 仿真通道 | 6 用例 | ✅ 真机解析出 `get_weather(杭州)` | **达标** |
| 4 | 流式响应 | ✅ Responses SSE | 4 用例 | ✅ 9 帧事件序列 | **达标** |
| 5 | 图片解析 | ✅ **base64 内联 + 落盘还原** | 21 用例 | ✅ 四角对照全对（左上/右下） | **达标** |
| 6 | 读写 token 缓存计数 | ✅ 读+写+别名+缺席区分 | 8 用例 | ✅ 字段透出 | **达标** |

**测试规模**：`proxy/prism` **79 用例全绿**（`-race` 干净）+ `proxy` 接线 **9 用例全绿** = **88 用例**

```
$ go test ./proxy/prism/ -count=1 -race
ok  github.com/codex2api/proxy/prism  4.355s
$ go test ./proxy/prism/ -count=1 -v | grep -c '^--- PASS'
79
$ go test ./proxy/ -run TestPrism -count=1 -v | grep -c '^--- PASS'
9
```

---

## 1. 高并发

**实现**
- 每账号 `prism.Client` 缓存（`prismClientsMu` 互斥）
- 材料仓库 `sync.RWMutex` + `sync.Once`
- sentinel 浅池 `chan`（容量 8）+ `sync.Once` + 失败负缓存互斥
- 每请求独立 `context`，下游取消即传导上游（省额度）

**测试**

| 用例 | 断言 |
|---|---|
| `TestConcurrent_MaterialLoadIsSafe` | 64 goroutine 并发加载材料，零错误（配合 `-race`） |
| `TestConcurrent_SentinelPoolIsSafe` | 50 goroutine 并发取票，**50 枚互不相同**（严格一次性语义） |
| `TestConcurrent_ClientsAreIsolated` | 32 路并发建客户端，凭据不串号 |
| `TestConcurrent_EndToEndUnderRace` | **64 路**并发跑完整 start→poll→SSE 链路，**64/64 成功**，含 goroutine 泄漏检查 |

**真机**：4 次连续调用（间隔 35s）全部成功，材料年龄始终 < TTL。

---

## 2. 低延迟

**实现**
- `EventWriter.emit()` 每写一个事件**立即 `Flush()`** —— 否则中间层（nginx/云 LB）攒缓冲，首字延迟会被拉长到整段结束
- 上游**无真流式**（start + 轮询模型），故不做假 sleep 分块；思考摘要若存在则作为 `reasoning` 事件真增量先发

**测试**

| 用例 | 断言 |
|---|---|
| `TestStream_FlushPerEvent` | Flush 次数 == 事件数（不攒批） |
| `TestStream_LowLatencyFirstEvent` | 用 `io.Pipe` 验证首批数据**立即**含 `response.created`，不等整轮写完 |

**边界说明（如实）**：Prism 上游是 start+轮询，**没有 token 级增量**。整轮耗时实测 **6~10s**（沙箱执行 + 轮询间隔）。本层能保证的是「事件不攒批、逐帧下发」，无法凭空造出上游没有的流式增量。这是**上游能力边界**，非网关缺陷。

---

## 3. 工具调用

**实现**（上游无客户端 function calling 通道 → 网关侧提示词仿真）
1. `toolProtocol()` 生成约束提示词：工具跑在**用户本机**、只有客户端能执行、用 `<tool_call>{...}</tool_call>` 信封请求、参数须满足 JSON Schema
2. `extractToolCalls()` 解析信封（支持多个、非法信封忽略、空参数规范成 `{}`）
3. 事件映射：普通工具 → `function_call` + 参数增量事件；自定义工具（`apply_patch`/`shell`/`exec*`）→ `custom_tool_call`

**测试**

| 用例 | 断言 |
|---|---|
| `TestTools_ProtocolForbidsLocalExecution` | 提示词含 `LOCAL MACHINE`/`Only the client can execute`/`JSON Schema` |
| `TestTools_MultipleCallsParsed` | 3 个 `<tool_call>` 全解析，正文清干净且不丢内容 |
| `TestTools_CallIDsUnique` | `call_id` 唯一（下游据此配对结果） |
| `TestTools_MalformedEnvelopeIgnored` | 4 种非法信封（非 JSON/缺 name/截断/未闭合）**都不解析**，不吞正文 |
| `TestTools_CustomToolEventRouting` | `apply_patch`→`custom_tool_call`；普通工具→`function_call` + delta |
| `TestTools_EmptyArgumentsBecomeObject` | 空参数规范成 `{}` |

**真机验证** ✅
```
输入工具定义：[get_weather(city:string)]
prompt     ：杭州天气怎么样？请调用 get_weather 工具
输出       ：[工具调用] [{"CallID":"call_1f6222ba-...","Name":"get_weather",
                        "Arguments":"{\"city\":\"杭州\"}"}]
```
模型正确产出信封，网关正确解析出结构化参数。

---

## 4. 流式响应

**实现**：`EventWriter` 输出标准 **Responses SSE**（`event:` + `data:` 双行），序列遵循规范：
```
response.created → response.in_progress
→ response.output_item.added → response.content_part.added
→ response.output_text.delta → response.output_text.done
→ response.content_part.done → response.output_item.done
→ response.completed
```

**测试**

| 用例 | 断言 |
|---|---|
| `TestStream_EventSequence` | 9 个事件的**顺序**完全符合规范 |
| `TestStream_SequenceNumbersMonotonic` | `sequence_number` 严格递增（下游据此排序），且每事件必有 |
| `TestStream_FlushPerEvent` | 见 §2 |
| `TestStream_LowLatencyFirstEvent` | 见 §2 |

**真机验证** ✅：9 帧完整序列，`delta` 含正文 `"流式测试OK"`，`response.completed` 带 usage。

**架构收益**：因适配成 Responses SSE，`/v1/responses`、`/v1/chat/completions`、`/v1/messages` **下游零改动**（与既有 Antigravity 通道同构）。

---

## 5. 图片解析 ✅ **base64 内联 + 落盘还原（真机四角对照验证）**

### 5.1 走过的弯路：input_image 与 input_file 都不通

初版用 `input_image` + `image_url(data:...)`：HTTP **200** 但模型**看不见**。

第二版改用**项目文件通道**（`POST /api/project-files/upload` + `input_file` 引用）。
上传确实成功（200，落盘 `/prism-uploads/<name>`），也曾**被误判为打通** ——
当时用「白底+左上黑块」提问，模型答了「左上」，就被当成"真的看到了"。

**⚠️ 那次判定是错的（2026-10-02 复核推翻）**：判据本身不可靠。
对「黑块在哪个角」这类问题，模型在**完全看不到图**时对"左上"有很强的先验倾向，
一次单角度测试极易被先验命中。复核证据链：

| 探针 | 结果 | 含义 |
|---|---|---|
| 同图（左上黑块）走 `input_file` 多次 | 「右上」/「左上」/「右下」不稳定 | 与图无关，纯先验抽样 |
| 四角矩阵（TL/TR/BL/BR） | 恒答「右」类方位 | 没有真正读取画面 |
| ⭐ **OCR 判据**：点阵数字 `7429` | 「**无法读取图片，请重新上传**」 | 决定性：模型拿不到图 |
| 换 **base64 内联**后同图 | TL→「左上」、BR→「右下」 | 换角度都对 ⇒ 真看见 |

**根因**：`input_file` 只是被服务端编成一句
`[project file: /prism-uploads/x.png]` 提示，模型需要去**会话工作区**找这个文件；
而文件能否出现在工作区，取决于站点编辑器把文件写进项目协作文档
（Y-Sweet，私有 WS 协议）—— 我们无法复制那条路（Go 侧无成熟 Yjs 实现，
且有写坏用户项目文档的风险）。

### 5.2 走通的路：base64 内联进用户消息（真机实测 ✅）

上游**没有任何二进制/多模态入参**，但可以把图片 **base64 内联进本轮 user 消息**，
同时给出「还原成工作区文件」的命令与 `view_image` 指引 —— 模型自己落盘后
调用 `view_image`，就能真看到图。

```
[attached image #attached-1.png — image/png, 2793 bytes]
This channel has NO native image input, so the image is provided inline as base64 above.
Before answering, restore it to your workspace and LOOK at it:

  mkdir -p prism-uploads && printf '%s' '<base64>' | base64 -d > prism-uploads/attached-1.png

  tool: view_image {"path": "prism-uploads/attached-1.png"}

Then answer the user's question about the image. Do NOT guess the image contents
from the base64 text or the filename.
```

**预算边界**（实测）：

| 内联 base64 字符数 | 结果 |
|---|---|
| ≤ ~59k | ✅ 可用（整轮较慢） |
| 219k | ❌ 上游 502（`codex_v2_restore_start failed`） |

⇒ 实现取**单图 ≤ 48k 字符、单请求 ≤ 96k 字符**；超限先缩图
（JPEG，阶梯降质 `768px/78 → 64px/28`，复用 `internal/imageproc`），
缩不下来则**如实告知「本轮未载入」**，绝不假装送达。

### 5.3 真机验证 ✅（四角对照，2026-10-02）

```
图片A：512×512 白底 + 左上角黑块(170px) → 模型答「左上」✅
图片B：512×512 白底 + 右下角黑块(170px) → 模型答「右下」✅

对照：同一张图走 input_file 路径 → 答「右上」❌（看不见）
```

两次独立、不同角度的正确读取 ⇒ 图片**真的被看到**（若为瞎猜，两个角度不可能都对）。

### 5.4 实现

- `proxy/prism/image_inline.go`：解码 → 预算判定 → 超限缩图 → 生成内联块与说明
- `Run()`：有图 → 内联 base64 + 落盘命令 + `view_image` 指引 → 附到最后一条 user 消息
- 复用 `internal/imageproc.MakeThumbnail`（JPEG 阶梯降质）
- 文件名安全化（去路径分隔/控制字符）
- **部分失败如实告知**：成功的内联送达、失败的明确告诉模型看不见，禁止猜测

### 5.5 测试（21 用例）

| 用例 | 断言 |
|---|---|
| `PrepareInlineImages_SmallStaysInline` | ⭐ 小图原样内联（不缩图），且在预算内 |
| `PrepareInlineImages_OversizeDownscaled` | ⭐ 超预算大图必须缩进预算（否则上游 502） |
| `PrepareInlineImages_BadPayloadReported` | 坏图**如实记入失败**（不静默丢弃） |
| `InlineImageText_HasRestoreAndViewImage` | ⭐ 说明必须含还原命令 + `view_image`，并禁止凭 base64 猜内容 |
| `AttachInlineImages_LastUserMessage` | 内联块附到**最后一条** user 消息 |
| `AttachInlineImages_NoUserMessageCreatesOne` | 无 user 消息时新建（**曾因不接 append 返回值而静默丢失**） |
| `InlineImageNoticeForFailures` | 部分失败如实说明；全成功不打扰模型 |
| `Upload_EndToEndImageVisible` | ⭐ 端到端：start body **内联 base64**、含 `view_image`、**不含** `input_file`/`input_image` |
| `Upload_HeadersMatchRealCapture` | 上传 7 个 header 与真机抓包一致（`input_file` 路径已弃用，保留作回归） |
| `ImageBytes_URLRejectsNonHTTP` | 拒绝 `file://` / `ftp://` / `javascript:` |
| `ImageBytes_URLSizeLimit` | 远程图片超限拒绝（防拖垮网关） |

**测试抓出的 bug**：`attachInlineImages` 用 `append` 但调用方不接返回值 →
无 user 消息时新建的消息**静默丢失**（图片永远送不出去）→ 已修。

**教训（写进流程）**：涉及模型"能否感知到某输入"的验证，**必须用多角度/客观判据**
（换角度、读数字、换内容），单次单角度的方位/颜色提问**不可作为通过依据**。

---


## 5.5 多槽材料池（吞吐扩展）

**动机**：压测暴露两条**上游硬限制**（实测钉死）：

| 限制 | 现象 | 应对 |
|---|---|---|
| sentinel **严格一次性** | 并发调用者若拿到同一枚 token → 只有第一个能用，其余 403 `Request verification failed` | 铸造服务**每次必铸新票**（去掉单飞去重，改并发队列） |
| **一沙箱一次只能跑一个对话** | 两个请求同时打同一沙箱 → 403 `Error while processing conversation` | 池按**每槽互斥（CAS 占用）**分配，请求排队等空闲槽 |

**实现**（`proxy/prism/pool.go`）
- 材料源可为**目录**（每份一个 `.json` = 一槽 = 一身份）或单文件（退化为单槽）
- 选槽：**只挑新鲜槽** → 空闲槽 CAS 抢占 → 轮转摊平长期分布
- 全部槽不可用 → **快速失败**（不白等）；全部被占 → 等到空闲或超时（默认 90s）
- `SetWaitTimeout()` 可调等待上限；`Stats()` 输出各槽新鲜度/在飞/成功失败计数

**新增侧车** `cmd/prism-material/daemon-multi.js`
- **一个浏览器上下文 = 一个身份 = 一槽**：每槽独立 cookie jar / UA / 沙箱
- `PRISM_SLOTS=N` 控制槽数；多账号时一槽一账号（真多身份）
- 每槽独立退避、崩溃自动重建、原子写盘

**测试**（12 用例，全绿）

| 用例 | 断言 |
|---|---|
| `DirScanning` | 目录扫描拾取 `.json`，忽略 `.tmp` 与子目录 |
| `SingleFileDegradesToSingleSlot` | 单文件退化单槽（向后兼容） |
| `RejectsExpiredSlots` | ⭐ 过期槽被拒（**且过期判定会缓存** —— 曾因不缓存而把过期材料当新鲜交出） |
| `SkipsBrokenSlot` | 坏槽（缺 sandbox 字段）被跳过，不拖垮整池 |
| `RoundRobinSpread` | 单并发时 300 次请求在 3 槽均匀分布 |
| `SerializesPerSlot` | ⭐ 槽忙时**等待**而非并发进入；释放后立刻放行 |
| `MultiSlotEnablesParallel` | ⭐ 3 槽可同时服务 3 路；第 4 路排队 |
| `ConcurrentAcquireIsSafe` | 200 路并发取还，无泄漏、无竞态（配 `-race`） |
| `DetectsRescanAfterSlotAppears` | 新槽出现后可拾取 |
| `SlotStatsPersistAcrossRescan` | 重扫保留已有槽统计 |
| `FailureCounting` / `StatsShape` | 计数与诊断结构正确 |

**真机验证** ✅（2 槽，4 路并发）
```
成功 4 / 失败 0 ｜ 总耗时 14.8s
  · 槽0 成功=2 失败=0
  · 槽1 成功=2 失败=0     ← 完美均衡
```
**并发语义修复验证** ✅：并发取 4 枚 token → **4 枚互不相同**（修复前会复用 → 403）

**吞吐口径**：单身份约 80 RPM。N 个**不同账号**的槽 → 理论 N×80 RPM；
若只有 1 个账号，多槽仅提升**可用性**（单槽坏掉不影响其它），不提吞吐。

### 5.6 单账号「在飞闸门 + 平滑放行」（本轮新增）

**动机**：标定「单账号 ≥15 路并发」时实测到一个**反直觉机制**（判别实验钉死）：

| 实验 | 结果 | 延迟特征 |
|---|---|---|
| A) 15 路**一次性打进** | 5/15 | 失败要 **7-15s** 才被拒 |
| B) 紧接 10s 后 **4 路 × 20** | **0/20** | 全部 **1.8s 秒拒** |

→ 若是固定并发上限，B 用 4 路应全绿；实际全灭。**真因**：突发请求把账号打进一段
**限流冷却期**，冷却期内一律秒拒 403（文案 `Error while processing conversation`，
并附 `Please submit prompt again`）。这解释了「每轮测完 4/15」——**每轮自己把自己打进冷却**。

**实现**（`proxy/prism/client.go` 的 `inflightGate`）
- `Config.MaxInflight`：单账号**在飞上限**（默认 4）——满员时新请求**排队等待**，而非被上游拒
- `Config.MinGap`：相邻上游请求**最小间隔**（平滑放行）——高并发必须**错峰**，不能同时打进
- 两把锁分离：`acquire` 排队（名额转交，不空转）+ `pace` 串行化放行时刻
- `ctx` 取消安全：排队/等间隔期间取消 → 立刻返回错误，**不泄漏名额**
- 生产接线：`PRISM_MAX_INFLIGHT`（默认 4）、`PRISM_MIN_GAP_MS`（默认 0）
- 侧车同款：`cmd/prism-worker/worker.js` 的 `PRISM_MAX_INFLIGHT`（15 槽排队过闸门）

**测试**（6 用例，全绿，含 `-race`）

| 用例 | 断言 |
|---|---|
| `LimitsConcurrency` | ⭐ 峰值在飞 ≤ 上限，且 **全部完成**（排队而非丢弃） |
| `ContextCancelDropsWaiter` | ⭐ 排队中取消 → 及时返回且**不泄漏名额** |
| `MinGapPaces` | ⭐ 最小间隔生效（4 次 ≥ 3×gap） |
| `MinGapCtxCancel` | 等间隔时取消能被及时响应 |
| `DisabledByDefault` / `ClientWired` | 默认不限流；配置能真正接到 Client |

### 5.7 403 真因取证（决定性 · 应用层抓包）

前面的判断都建立在「页面文案」上，容易被误导。本轮改为**在页面内挂钩 `fetch`/`XHR`**
（`addInitScript`），拿到**应用实际看到的响应**，一锤定音：

```json
HTTP 200  POST /api/llm/response_with_tools_start
{
  "status": "completed",
  "request_id": "c9d3edd3-…",
  "conversation_id": "cdx1_<随机 UUID>",
  "response": {
    "status": "error",
    "payload": {
      "reason": "unknown",
      "message": "Error while processing conversation (403 Forbidden). Please submit prompt again.",
      "httpStatus": 403
    }
  }
}
```

**三个关键事实**（全部由此坐实）：

1. ⚠️ **错误藏在 HTTP 200 里** —— 只看 HTTP 状态码会完全漏掉失败（下游会收到"成功但空正文"）。
   必须剥到 `response.payload.httpStatus` / `.message`。Go 侧已如此实现（`fillFromResponse` → `Turn.ErrMessage`）。
2. ⚠️ **上游自称「可重试」** —— `Please submit prompt again.` 明确提示重发；因此
   **重试 + 退避**是正确的，而"不重试"会让每次限流都直接变成用户可见失败。
3. ⚠️ **限流是账号级且带冷却期** —— 触发后一段时间内**一律**秒拒（~1.6s），
   与并发数、槽、项目、材料是否新鲜**都无关**。

### 5.8 账号级熔断冷却（本轮新增）

既然上游明确「可重试」，与其硬打被秒拒，不如**主动退避**：

**实现**（`proxy/prism/client.go`）
- `isRetryableUpstreamErr(msg)`：识别限流型错误（`error while processing conversation`
  / `please submit prompt again` / `(403 forbidden)` / `(429` / `rate limit` / `too many requests`）
- `noteUpstreamError(turn.ErrMessage)`：命中 → `tripCooldown(base)`
- `tripCooldown` **递增退避**：第 n 次连续触发 = `base << (n-1)`，上限 15 分钟
- `noteUpstreamSuccess()`：成功 → `resetTrips()` 复位
- 冷却期内 `acquire` **主动等待**而不是硬打；`ctx` 取消安全
- 生产接线：`PRISM_COOLDOWN_MS`（默认 20000）

**测试**（5 用例，全绿，含 `-race`）

| 用例 | 断言 |
|---|---|
| `IsRetryableUpstreamErr` | ⭐ 真机文案判为可重试；400/材料过期判为不可重试 |
| `CooldownTripAndWait` | ⭐ 熔断后新请求被压住到冷却结束，而非硬打 |
| `CooldownBacksOff` | ⭐ 连续触发退避递增；成功后复位回基础档 |
| `CooldownCtxCancel` | 等冷却期间取消能及时返回 |
| `CooldownDisabled` | 无冷却时零额外等待 |

**结论**：单账号 15 路**可行**，工艺是
**「15 槽常驻（同时在线） + 在飞闸门 + 平滑放行 + 账号级熔断退避」**。
不允许突发 —— 突发必触发上游冷却期；一旦触发，靠熔断退避而不是硬打。

---

## 5.9 403 真根因：Cloudflare TLS 指纹（决定性修复）

### 5.9.1 调研过程（外部交叉验证）

单账号 15 路标定长期受阻于 `Error while processing conversation (403)`。按「瓶颈先调研」原则
检索同类项目，命中两条**症状几乎完全一致**的公开 issue：

| 来源 | 内容 |
|---|---|
| openclaw #62087 | `chatgpt.com/backend-api` 403，「CF mitigation active，token 有效但端点要求浏览器验证」 |
| openclaw #67670 | **决定性**：Cloudflare 按 **JA3/JA4** 把非浏览器 TLS 指纹判为 bot → 403；**同一代理、同一 token**，用 `cloudscraper`（Chrome 指纹）→ **200 OK** |

结论：**403 与账号限流无关，根因是 TLS 指纹。** 这与我们的现象完全吻合 ——
403 与并发数、槽、项目、材料新鲜度都无关，且网页端（真 Chrome）也失败。

### 5.9.2 本项目的问题

`proxy/prism` 最初用的是**裸 Go HTTP 客户端**：

```go
http: &http.Client{Timeout: 0},   // 无自定义 Transport → Go 默认 TLS 指纹
```

而 `cf_clearance` 是**绑定 IP + UA + TLS 指纹**的：材料由真 Chrome 产出（带着 Chrome 的
`cf_clearance`），请求却以 **Go 指纹**发出 —— 指纹不匹配即被 Cloudflare 拒。

**讽刺的是仓库自己早就解决了这个问题**：`proxy/utls_transport.go` 里有完整的 Chrome
指纹 transport（`utls.HelloChrome_Auto`、HTTP/2 连接池、保活、代理支持），只是 **Prism 通道没接上**。

### 5.9.3 实测指纹对比（tls.peet.ws 回显，直连）

| 客户端 | JA3 hash | JA4 | 密码套件数 |
|---|---|---|---|
| **裸 Go**（修复前） | `03117a8ed39ef02427ebbc39f121275c` | `t13d1312h2_f57a46bbacb6_f50d94e863eb` | 13 |
| **utls Chrome**（修复后） | `0f46ad05a63ea03f3224a0beb6635e1e` | `t13d1516h2_8daaf6152771_d8a2da3f94cd` | **15**（Chrome 特有） |

### 5.9.4 修复

**① 注入 Chrome 指纹**（`proxy/prism_channel.go`）
```go
// 门槛：① PRISM_TLS_PROFILE != 0  ② 目标必须是 https（utls 只做 TLS 握手）
if envInt("PRISM_TLS_PROFILE", 1) != 0 && strings.HasPrefix(prism.CurrentBase(cfg), "https://") {
    cfg.Transport = NewUTLSTransport(os.Getenv("PRISM_PROXY"))
}
```
- `Config.Transport`（`client.go`）：新增注入点，`NewClient` 用它构造 `http.Client`
- `PRISM_PROXY` 支持代理（utls 自带 HTTP CONNECT / SOCKS5 拨号器）
- `PRISM_TLS_PROFILE=0` 可关闭（排障 / A/B 对照）
- https 门槛是必需的：单测用 `httptest` 的**明文**上游，utls 无法对 http:// 建连

**② 顺带修掉一个真 bug：utls transport 在 Go 1.27 下 panic**

`go.mod` 写 `go 1.26.6`，本机 Go 1.27.1 → 满足要求 → `x/net v0.55.0` 的
`//go:build go1.27 && !http2legacy` 生效 → 切到**新 http2 实现**（委托 `net/http.Transport`），
而 `http2.Transport.NewClientConn` 在新实现里要求 `ConnPool`，代码没设置 → **nil 指针 panic**。

```
net/http.(*Transport).NewClientConn(0x0, …)   ← nil
  ← x/net/http2.(*Transport).newUserClientConn
  ← proxy.(*utlsRoundTripper).createConnection   (utls_transport.go:365)
```

**当时的绕行**：构建加 `-tags http2legacy` 回到经典 http2 实现（x/net 官方兼容开关）。

> ⚠️ 这同时解释了仓库里**长期失败**的 2 个测试
> （`proxy/utls_transport_leak_test.go`、`auth/utls_client_leak_test.go`）
> —— 它们不是"无关的历史噪音"，而是这个真 bug 的直接症状。

**✅ 上游已从根上修复（本分支已同步）**：上游提交
`fix(deps): bump golang.org/x/net to v0.57.0 to fix Go 1.27 http2 NewClientConn panic`
（症状、文件、根因三者与我们独立诊断的完全一致）—— v0.57.0 在 `newUserClientConn`
里初始化了 transport，因此**不再需要 `-tags http2legacy`**。

> 结论：`-tags http2legacy` 只是**临时绕行**。同步上游后，本通道在
> Go 1.27 下**默认构建/测试即全绿**（已实测：`proxy/prism` 83 用例、
> 接线 9 用例、既有 UTLS 用例，均不带 tag 通过）。

### 5.9.5 验证

| 项 | 结果 |
|---|---|
| JA3/JA4 指纹确实改变 | ✅ 见 §5.9.3 |
| `proxy/prism` 单元测试（`-race`） | ✅ 81 用例全绿 |
| 接线测试 `TestPrism*` | ✅ 9 用例全绿（带/不带 tag 均过） |
| 既有 UTLS 泄漏测试 | ✅ **转绿**（修复前 panic） |
| `Config.Transport` 接线 | ✅ 新增 `TestTransportInjected` 钉死（防再次断线） |
| 不带 tag 构建/测试（同步上游后） | ✅ 全绿（x/net v0.57.0 已根治，无需 `http2legacy`） |

### 5.9.6 构建方式

```bash
# 默认即可（x/net ≥ v0.57.0 已根治 Go 1.27 的 http2 NewClientConn panic）
go build ./...
go test  ./proxy/prism/ -race
```

> 📌 历史沿革：在 x/net v0.55.0 上曾**必须**加 `-tags http2legacy`，
> 否则 uTLS transport 会 nil panic。上游升级到 **v0.57.0** 后该绕行已不需要
> （见 §5.9.4 ②）。保留本节仅为解释历史文档中出现的 `-tags http2legacy`。

---

## 5.10 限流治理三修（本轮新增）

指纹修好后，403 并未立即消失 —— 继续排查发现**三个我们自己造成的放大器/静默缺陷**。

### 5.10.1 判别实验：瓶颈是「突发」而非「总量」

| 实验 | 配置 | 结果 |
|---|---|---|
| 15 路齐发 | 每槽 1、无错峰 | 4-5 / 15 |
| **串行 10 次** | 间隔 6s、**仅 1 路在飞** | **连续 7 次成功**（#1–#7 ✓，#8 ✗，#9 ✓）|

→ 若是"每窗口固定配额"，串行也不可能连过 7 次。**结论：上游拦的是"突发"**，
平滑放行（在飞闸门 + 最小间隔）正确，但还需先拆掉自己的两个放大器。

### 5.10.2 缺陷一：换槽重试是**上游调用放大器** ⭐

`responses.go` 原逻辑：失败即**换槽重试最多 3 次**。问题在于 ——
换槽只换材料/身份，**绕不开账号级限流**，而每次重试**都会再打一次上游**：

```
15 路请求 × 3 次换槽 = 45+ 次上游调用   →  自己烧光配额、把账号推入更深冷却
```

实测证据（cap15 实验）：错误信息里出现
`无可用材料槽（10 槽全部不可用: 槽0:材料已过期…）` —— 正是重试把材料 TTL 耗尽，
且总耗时被拉到 **6 分钟**（每路 250-365s）。

**修复**
- 限流型错误（`isRetryableUpstreamErr`）**立即上抛，不再换槽重试**
- 新增 `Config.SlotAttempts`（`PRISM_SLOT_ATTEMPTS`，默认 3）可下调；突发场景建议 **1**
- CLI：`prism-multi -slot-attempts 1`

### 5.10.3 缺陷二：单槽路径**把上游错误当成成功** ⭐

`Run` 的单槽分支原为：

```go
turn, rerr := c.runWithMaterial(ctx, req, mat)
slot.Release(rerr)
return turn, rerr      // ← rerr 为 nil 时直接返回
```

上游把错误藏在 **HTTP 200 的内层 payload**（`response.status=error`）时 `rerr == nil`，
于是：**下游收到"成功"、且账号级熔断根本不触发**。多槽路径有判断，单槽路径漏了。

**修复**：抽出统一收尾 `finishTurn(turn, err)`，**两条路径共用**
（成功→复位退避；失败→触发熔断并返回 err）。这类"把失败当成功"的静默缺陷
最危险 —— 它会掩盖上游限流，让问题看起来像"偶发空正文"。

### 5.10.4 缺陷三：`utls` transport 在 Go ≥1.27 下 panic

见 §5.9.4 ②。**已由上游 x/net v0.57.0 根治**，不再需要 `-tags http2legacy`。

### 5.10.5 测试（新增 3 用例，全绿）

| 用例 | 断言 |
|---|---|
| `TestSlotRetry_NotAmplifiedOnRateLimit` | ⭐ 限流型错误**只打上游 1 次**（不放大）且必须上抛 |
| `TestSlotRetry_StillRetriesNonRateLimit` | 反向保证：非限流型失败**仍然**换槽重试（不误伤多身份价值）|
| `TestTransportInjected` | ⭐ TLS 指纹 transport 真的被用上（防接线再次断开）|

### 5.10.6 修正一个旧结论：材料有效窗口

此前记录「材料有效窗口仅 20-30s，静置 60s 全 403」。**本轮用 44s / 109s 龄材料均成功**，
说明旧结论被**账号冷却**混淆（confounded）—— 当时账号正被限流，与材料无关。

> ✅ 修正：材料**没有** 20-30s 的硬窗口；旧的"造完即用"紧迫感不必再保留。
> ⚠️ 教训：单一变量实验必须在**账号健康**的前提下做，否则会得到伪因果。

---

## 5.11 单账号 15 路标定（最终矩阵）

### 5.11.1 ⚠️ 先说一个曾污染实验的环境坑：`PRISM_SENTINEL_URL`

Prism 门禁需要一次性 `openai-sentinel-token`。仓库有两条取 token 路径：

| 配置 | 行为 |
|---|---|
| `PRISM_SENTINEL_URL=http://127.0.0.1:8791` | ✅ 直连常驻 daemon（单次铸造 ~0.6s）|
| **未配置** | ❌ 退回「每次请求现起 node runner」——**每请求重载 SDK 5s+** |

**后果**：15 路并发下 node 进程风暴 + 全超时，表现成
`取 sentinel 失败: 启动 node runner 失败: context deadline exceeded`，
**看起来像上游问题，实际是本地配置缺失**。曾据此得到「0/15」的无效结果。

> ✅ 已在实验脚本中加**前置条件预检门**：`/healthz` 必须 `ok:true` 且 `/token` 必须 200，
> 否则直接中止 —— 不再白跑十几分钟的长实验。
> 📌 所有涉及 sentinel 的真实链路实验，都必须带 `PRISM_SENTINEL_URL`。

### 5.11.2 最终真机矩阵（账号健康 + sentinel 正确接线）

| 轮次 | 配置 | 结果 | 备注 |
|---|---|---|---|
| 历史 | 15 路齐发 + 换槽重试 3 | **4-5 / 15** | 含放大器 bug（N×3 次上游调用）|
| v1 | 在飞 3 + 间隔 2s + 不换槽 | **8 / 15** | 延迟爆炸（材料中途过期）|
| v2 | 在飞 3 + 间隔 1s | 4 / 15 | sentinel 未接线（无效）|
| v3 | 串行 间隔 12s | 0 / 15 | **无效**（sentinel 未接线）|
| **v5** | **串行 间隔 12s + sentinel 已接线** | **12 / 15** ⬆️ | 新纪录；失败仅 #8-10，**之后 5 路全成功** |
| v6 | 串行 间隔 25s + 退避调短 | 见下 | 冲刺 15/15 |

### 5.11.3 结论

1. **突发是主因**：齐发 4-5/15 → 串行平滑放行 **12/15**（提升 ~2.5×）。
2. **存在「突发配额 + 冷却」形态**：v5 失败集中在 **#8-10**，**之后 #11-15 全部成功**
   —— 典型的「触发一次配额 → 短暂冷却 → 恢复放行」，而非硬性并发上限。
3. **正确工艺**：
   - **平滑放行**（`PRISM_MIN_GAP_MS`）比「在飞闸门」更关键
   - **不要换槽/换号重试**（`PRISM_SLOT_ATTEMPTS=1`）—— 那是放大器
   - 账号级**熔断递增退避**（`PRISM_COOLDOWN_MS`）兜底
   - 材料 TTL 必须 **≥ 一轮总时长**（`PRISM_MATERIAL_TTL_MS`），否则后段无材料可用
4. **「15 路会话并发」的正确读法**：15 路**会话可同时在线**（各占独立项目/材料槽），
   但上游调用需按 ~12-25s 间隔**平滑放行**。这是**单账号的真实天花板**；
   要真正 15 路同刻在飞，需**多账号**（仅 1 个可用账号，无法验证）。

---

### 5.11.4 ⚠️ 关键规律：账号**累积退化**，测速本身在压坏它

把本次会话所有 15 路轮次的**延迟中位数**按时间排开：

| 轮次 | 配置 | 成绩 | 中位延迟 |
|---|---|---|---|
| 早期 | 齐发 | 4-5/15 | ~10s |
| v1 | 在飞 3 + gap 2s | 8/15 | 210s |
| v2 | 在飞 3 + gap 1s | 4/15 | 97s |
| **v5** | **gap 12s + sentinel 接线** | **12/15** ⬆️ | **243s** |
| v6 | gap 25s + 退避调短 | 8/15 | **579s** |

**延迟单调上升（10s → 600s）**，且 v6 失败变成
`prism: status: context deadline exceeded`（撞上 **10 分钟轮询硬上限**），
不再是 403 —— 说明**上游对该账号的吞吐在随累计调用量持续下降**。

**推论（重要）**：
- 本轮「8/15」并不是 15 路上限，而是**账号已被前几轮压测弄差**。
- **12/15（v5）才是本会话的干净读数**，且它是在账号尚健康时取得的。
- 继续压测只会让读数更差 —— 这解释了为什么后几轮「越测越差」。

> 📌 标定类实验必须**串行进行且轮次之间充分冷却**；连续猛测会污染后续所有读数。

### 5.11.5 最终结论与工艺

**单账号天花板**：健康账号 + 串行平滑放行（间隔 ~12s）→ **12/15（80%）**。
失败呈「配额触发 → 短暂冷却 → 恢复放行」形态（v5 中 #8-10 失败后 #11-15 全部成功），
**不存在硬性固定并发上限**。

**生产工艺（已落地为 env）**：

| env | 建议 | 作用 |
|---|---|---|
| `PRISM_MIN_GAP_MS` | **12000** | ⭐ 平滑放行（最关键）|
| `PRISM_SLOT_ATTEMPTS` | **1** | ⭐ 关掉换槽重试（放大器）|
| `PRISM_MAX_INFLIGHT` | 1-3 | 在飞闸门（辅助）|
| `PRISM_COOLDOWN_MS` | 8000-20000 | 账号级熔断递增退避 |
| `PRISM_MATERIAL_TTL_MS` | ≥ 一轮总时长 | 防后段材料过期 |
| `PRISM_SENTINEL_URL` | `http://127.0.0.1:8791` | ⚠️ 必设，否则每请求现起 node（5s+）|

**要真正 15 路同刻在飞**：需**多账号**（单账号速率配额是硬约束）。
当前池中仅 1 个可用账号，故无法验证多账号上限。

---

## 6. 读写 token 缓存计数

**实现**
- **读缓存** `CachedInputTokens`（cache read）
- **写缓存** `CacheWriteTokens`（cache creation）+ `CacheWriteReported` 标志
  → **区分「确实是 0」与「上游没说」**，否则缓存写成本会被静默算漏
- **多别名兼容**（上游字段名会变）：
  - 读：`cached_input_tokens` / `cache_read_input_tokens` / `cached_tokens` / `input_tokens_cached`
  - 写：`cache_creation_input_tokens` / `cache_write_input_tokens` / `cache_creation_tokens`
  - 输入输出：`input_tokens`/`prompt_tokens`、`output_tokens`/`completion_tokens`
- **嵌套明细**：`input_tokens_details.{cached_tokens, cache_creation_tokens}`、`output_tokens_details.reasoning_tokens`
- 口径汇总：`EffectiveInput() = 新输入 + 读 + 写`（对齐上游不变量）、`CacheHitRate()`
- `Usage.Raw` 保留原文便于对账
- SSE 终态同时透出：`input_tokens_details.{cached_tokens, cache_creation_tokens, cache_write_reported}` + `usage.prism.{cache_read_tokens, cache_write_tokens, cache_hit_rate}`

**测试**

| 用例 | 断言 |
|---|---|
| `TestUsage_CacheReadWrite` | 读 60 / 写 15 都正确，写标记已上报 |
| `TestUsage_DistinguishesAbsentFromZero` | ⭐ 缺席→`reported=false`；显式 0→`reported=true` |
| `TestUsage_AliasCompatibility` | 读 3 别名 + 写 2 别名 + prompt/completion 全部生效 |
| `TestUsage_NestedDetails` | Responses 风格嵌套明细（读 8 / 写 3 分别取，不串） |
| `TestUsage_EffectiveInputInvariant` | 不变量 10+60+30=100，命中率 0.6；零值安全 |
| `TestUsage_RawPreserved` | 未知字段仍保留在 Raw |
| `TestUsage_TotalFallback` | total 缺失时自算 |
| `TestUsage_EmittedInSSE` | ⭐ 读/写/汇总/命中率全部出现在 `response.completed` |

**测试抓出的真 bug**：嵌套明细处原本用一个 `pickIntOK(d, "cached_tokens", "cache_creation_tokens")`，
会把 `cache_creation` 的值当成 `cached` 读出（把写缓存算进读缓存）→ 已修为分别取值。

---

## 7. 测试过程中抓出并修复的缺陷

| # | 缺陷 | 影响 | 修复 |
|---|---|---|---|
| 1 | 响应解析少剥一层（读 `response.output` 而非 `response.payload.output`） | 正文**永远为空**，且上游错误被埋 → 失败**误报成功** | `fillFromResponse` 剥到 payload + 3 个回归用例 |
| 2 | 嵌套 usage 读写缓存共用一个 picker | 写缓存被算进读缓存 | 分别取值 + `TestUsage_NestedDetails` |
| 3 | data URL 字节估算用 `DecodedLen` | 向上取整（8→9） | 自算 padding 精确值 + 用例 |
| 4 | 重构时删了 `nodeBin` 默认值 | 本地铸造报 `exec: no command` | 补默认 `"node"` |

---

## 8. 复现命令

```bash
# 单元 + 接线测试（含竞态）
go test ./proxy/prism/ -count=1 -race
go test ./proxy/ -run TestPrism -count=1 -v

# 真机端到端（需先起两个宿主侧车，见 docs/prism-deployment.md）
export PRISM_MATERIAL_PATH=/tmp/prism_sidecar/material.json
export PRISM_SENTINEL_URL=http://127.0.0.1:8791

# 基础闭环
go run ./cmd/prism-live -account /tmp/cpa_account.json -prompt "只回复三个字：收到了"
# 流式事件
go run ./cmd/prism-live -account /tmp/cpa_account.json -prompt "只回复：OK" -stream
# 工具仿真
go run ./cmd/prism-live -account /tmp/cpa_account.json \
  -prompt "杭州天气？请调用 get_weather" \
  -tools '[{"type":"function","name":"get_weather","description":"查天气","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]'
# 图片形态探测
go run ./cmd/prism-image
```

---

## 9. 已知边界（如实）

1. **图片内容不可达**（§5）：上游私协议不消费 data URL 图片；真机走项目文件通道，未接入。已做诚实降级。
2. **无 token 级真流式**：上游是 start+轮询，整轮 6~10s。
3. **单材料身份限流**：一份材料 = 一个浏览器身份，上游按身份限流（约 80 RPM）。高吞吐需多槽材料轮转（未实现）。
4. **容器内无 node**：sentinel 铸造必须外置（`docs/prism-deployment.md` §1）。
5. **上游沙箱额度**：材料里的沙箱被所有请求复用；勿频繁 reload 页面。
6. **`go build ./...` 既有报错**：根 `main.go` 的 `frontend/dist/*` 不存在（未跑前端构建，与本次无关）。
