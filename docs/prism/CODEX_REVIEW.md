# Prism 独立代码与测试审阅

## 1. Meta

- 日期：2026-10-02，Asia/Taipei（UTC+8）。
- 模型：GPT-6 / Codex，当前会话；具体后端子版本与 reasoning effort 参数未向审阅者暴露，故不填写推测值。
- 审阅基线：HEAD `613b5af2ce2fe59a0d37bf34a0ee6104f4ec81be` **加当前工作树**，包含 staged、unstaged、untracked 的 Prism 工作；并非仅审阅已提交或已暂存版本。
- 范围：`internal/prismchannel/` 全部 15 个 Go 文件（含全部测试）、`proxy/prism_handler.go`、`proxy/prism_stream.go`、`proxy/prism_test.go`；`main.go`、`config/config.go`、`proxy/handler.go` 的启用与路由，以及相关鉴权、账号限制、usage 路径；`docs/prism/{TEST_REPORT,DESIGN,GOAL,RUNBOOK,DELIVERY,GOAL-PERF}.md`。
- 参考树：只读比较 `/workspace/codex-prism/oai-prism/internal/{prism,facade}/`、`chatgpt-prism2api/internal/adapter/prism/`、`prism-proxy/src/prism-client.mjs` 中的相关实现。参考实现是本地协议证据，不能保证当日上游所有部署完全一致。
- 辅助证据：只读检查 `tools/prism/live_perf.py`；从四份历史 live 日志提取白名单字段，未将原始响应、身份材料或凭据复制进报告。
- 变更边界：仓库仅新增本报告。诊断文件和执行输出在 `/tmp/prism-review-*`；生产代码、现有测试、参考树、git index 均未修改。未部署、重启服务、执行 live 请求或读取凭据文件。

### 执行命令与结果

```bash
go version
git rev-parse HEAD
git status --short
git diff --stat
git diff --cached --stat

GIN_MODE=release go test ./internal/prismchannel/... ./proxy/ \
  -run 'Prism|prism|Feature' -count=1 -v
GIN_MODE=release go test -race ./internal/prismchannel/... ./proxy/ \
  -run 'Prism|prism|Feature' -count=1

# /tmp 中的独立诊断，通过 Go overlay 加入编译视图，仓库不增加测试文件：
GIN_MODE=release go test -overlay /tmp/prism-review-diagnostics/overlay.json \
  ./internal/prismchannel/... ./proxy/ -run '^TestPrismReview' -count=1 -v
GIN_MODE=release go test -overlay /tmp/prism-review-diagnostics/overlay.json \
  ./internal/prismchannel/... -run '^TestPrismReviewLocalCachedTokens' -count=1 -v
```

| 检查 | 本次结果及边界 |
|---|---|
| 普通单测 | PASS：channel 28 个顶层 / 含子测试 43 个；proxy 5 个 Prism 测试，另因 `Feature` 正则选中 2 个现有非 Prism 测试，也通过 |
| 同范围 `-race` | 两个包 PASS；无此次执行检测到的数据竞争 |
| 独立缺陷诊断 | 8 个诊断全部复现预期问题；这些测试的 PASS 表示“缺陷已复现”，不是产品正确性通过 |
| 初始受限环境 | 普通测试与 race 初次均因 `httptest` 的 loopback listener 被沙箱禁止而失败；允许本地监听后复跑成功，不能把初次结果算成代码失败 |
| 凭据模式扫描 | 27 个范围内代码/文档文件：未发现 JWT、常见真实 token 前缀或私钥标记；4 份历史证据日志未发现 JWT/私钥标记。只是模式扫描，不等于所有敏感数据审计通过 |
| 未复跑 | 全仓 `go test ./...`、vet、build、live matrix、持续压测；DELIVERY 对这些项目的 PASS 是历史声明，不是本次独立确认 |

输出保存在 `/tmp/prism-review-test-escalated.log`、`/tmp/prism-review-race-escalated.log`、`/tmp/prism-review-diagnostics/results.log`、`/tmp/prism-review-diagnostics/cache-result.log`。临时 overlay 源码位于 `/tmp/prism-review-diagnostics/{channel_test,proxy_test}.go`，全部使用显式 mock 材料。

**审阅结论：存在 3 项 High、7 项 Medium、3 项 Low；另列 2 项 Info。现有测试通过不能支持“production-grade / 六项功能无实质缺口”的验收结论。**

## Fix status (2026-10-02)

The findings below describe the original review baseline. Follow-up implementation
and mock/race verification are recorded in [CODEX_FIXES.md](CODEX_FIXES.md):
H1/H2/H3 and M1/M2/M4/M5/M6/M7 are fixed (some unsupported capabilities now fail
explicitly); M3 is partially fixed, with headless visibility and historical inline
budget prioritization still open. L1/L2 remain open; L3 documentation is corrected.
No new live verification or deployment was performed.

## 2. Findings by severity

### Critical

本次未确认 Critical 级别问题。未发现范围内真实凭据直接泄漏，也未确认可由客户端任意指定上游 origin 的 SSRF 路径；这不替代部署与真实材料审计。

### High

#### H1 — Prism 提前分支绕过账号分组、套餐及部分 scope 策略

**证据：** `proxy/handler.go:3827`、`:6750` 在 stock 调度路径前返回；`proxy/prism_handler.go:39` 只检查 channel=auto，`:60` 检查通用 key 限制，`:63` 取 key 并发位，`:122` 直接调用独立 Prism 池。`internal/prismchannel/client.go:102` 的选号只考虑 cooldown 和 slot ID，没有 group/plan 授权参数。

stock 的套餐/分组门在 `auth/store.go:9550`、`:9580`、`:9601`；这些不是 `enforceAPIKeyLimitsAndReply` 中的模型限制。`database/postgres.go:1799`、`:1816` 明确有 `AllowedGroupIDs` 和 `PlanAllow`。scope skip/filter 与 scope concurrency 在 `proxy/handler.go:6845`、`:6930` 等后续调度位置执行；Prism 不走这些位置。Prism usage 在 `proxy/prism_handler.go:157` 没有 `AccountID`，而 `proxy/apikey_scope_budget.go:293` 对 `AccountID<=0` 直接跳过 scope 计量。

**已复现：** 给 gin context 注入一个已鉴权形态的 auto-channel key，设置受限分组及不可能匹配的套餐白名单，`handlePrism` 仍返回 200，mock upstream 收到一次 start（`TestPrismReviewRestrictedGroupsAndPlansIgnored`）。此诊断验证策略分支，不宣称绕过鉴权；真正的 auth middleware 仍在路由前。

**影响：** 启用 Prism 会让原本受账号边界限制的 auto key 使用独立配置的 Prism 凭据；skip 型 scope 预算与账号/组并发限制也不能完整沿用。“preserving key policies”（`DESIGN.md:109`）过宽。普通模型白/黑名单、全局 key 限流与 key 并发门仍有效，不能一概说所有限制失效。

**修复：** 在 Prism 凭据有明确账号/组/套餐映射前，对带这些限制的 key fail closed；或将 Prism 接入统一授权、scope 并发及用量模型。仅检查 auto-channel 不足够。

#### H2 — 阻塞的 SSE 写可以越过请求 deadline，长期占用槽位

**证据：** `proxy/prism_stream.go:83`、`:87`、`:90` 同步写入并 Flush，无每次写入 deadline；`:118` 心跳也一样。`proxy/prism_handler.go:95` 在 `streamMu` 内写，`:124` 等待心跳 goroutine 退出。`internal/prismchannel/client.go:418` 的 timeout 只能取消 context，不能打断已经阻塞的 Writer；槽位在 `:458` 的 defer 才归还。`main.go:638` 的 HTTP server 有意不设固定 WriteTimeout，也没有给 Prism 单次写补上 deadline。

**已复现：** 用可控阻塞 Writer 接收第一帧，取消 request context 后 handler 仍不退出，`Inflight=1`；解除 Writer 阻塞后才归还（`TestPrismReviewBlockedWriteOutlivesRequestCancellation`）。这是本地阻塞写诊断，不是公网攻击压测。

**影响：** 慢读/停止读取的已授权客户端可让请求超时保护失效，消耗 Prism slot、key 并发位和 goroutine。若阻塞发生在心跳，`keepaliveWG.Wait()` 也会被拖住；单纯 context 取消与 upstream stop 不解决下游阻塞写。

**修复：** 给每次 SSE write/flush 设置可刷新写入期限（例如检查 Writer 能力后使用 ResponseController），失败时取消 upstream；同时确保心跳退出不会无限等一个不可中断的写。不要以给整个长回答设置固定总写超时替代逐次写期限。

#### H3 — 完整 material 模式会覆盖浏览器身份，破坏 metadata 自洽性

**证据：** `internal/prismchannel/session.go:97`、`:113` 保存 provider metadata；`client.go:509` 复制它，但 `:529` 无条件以 `s.credential.UserID` 覆盖 `userId`，`:528`、`:530`、`:531`、`:532` 也覆盖其他上下文字段。`config.go:150` 的环境变量凭据没有 UserID；material 模式又允许只给账号 ID / provider Cookie（`:167`）。因此正常配置很容易产生空 UserID。

**具体参考差异：** `/workspace/codex-prism/chatgpt-prism2api/internal/adapter/prism/client.go:954`–`:977` 强调捕获材料整套自洽；有材料时完整复制，只覆盖本轮 model/effort。本仓 `DESIGN.md:131` 也承诺“所有 metadata 保留，只覆盖 model/effort”，与代码不符。

**已复现：** provider 返回非空 mock `userId`，配置凭据未显式设置 UserID；发送到 start 的 `userId` 变为空（`TestPrismReviewMaterialUserIDOverwritten`）。现有 `session_test.go:71` 的 provider fixture 没有 userId，`:41` 只检查额外 snapshot/conversation 字符串，测不出这个覆盖。

**影响：** 完整 browser material 模式可因身份与 snapshot/project 不一致被上游拒绝。headers-only 部署的历史成功不能验证另一种配置模式。

**修复：** 完整 material 模式保留 browser identity/context；需要覆盖时先做显式一致性校验。无材料模式则应从已验证身份取得 UserID，或将其设为必需配置，避免默默发送空值。

### Medium

#### M1 — 将本地历史缓存估算冒充 OpenAI provider `cached_tokens`

**证据：** `internal/prismchannel/tokencache.go:93`、`:109` 按完整 items JSON 的 UTF-8 字节数估算缓存 token，里面包含历史 assistant 输出和图片 data URL，不是 tokenizer 结果，也不是上游 KV-cache usage。`client.go:438` 将其放入 Result，`proxy/prism_stream.go:16`、`:18` 无条件映射为标准 `input_tokens_details.cached_tokens` / `prompt_tokens_details.cached_tokens`。`types.go:71` 的上游 Usage 根本没有 provider cached-token 字段。

**已复现：** mock 提供 authoritative `input_tokens=24`；第二轮本地 `CacheReadTokens=542`，`prism_estimated=false`（`TestPrismReviewLocalCachedTokensExceedProviderInput`）。由上述映射可生成 `cached_tokens > input_tokens` 的不自洽响应。

**影响：** 客户端成本统计、缓存效果判断和账单分析可能误把重放历史当作上游缓存命中。标记 `prism_estimated=false` 只描述请求总量，对这项本地估算没有明确区分。当前 DB usage 又未写入对应 cached tokens（`prism_handler.go:157`），与 API 返回语义进一步分离。

**文档冲突：** `TEST_REPORT.md:161` 把从 0 改成这个值称作修复；`DESIGN.md:153` 和 `RUNBOOK.md:204`–`:209` 则正确写明本地重放不能宣称 provider cache 命中。

**修复：** 保留 `prism_cache_read_tokens` / `prism_cache_write_tokens` 为独立本地估算；标准 cached_tokens 使用确证的上游值，未取得时保守为 0。给本地估算单独标记，不能靠修改文档把标准字段重新定义。

#### M2 — 沙箱轮换后，旧 continuation snapshot 与新 sandbox metadata 混用

**证据：** `sandbox.go:90`–`:134` 在 TTL 到期后重新分配沙箱，但保留 project。`client.go:493` 只验证 prior.ProjectID，`:533`–`:536` 原样恢复旧 snapshot（含旧 sandbox_token/url），`:530`–`:531` 顶层 metadata 却使用新沙箱。`tokencache.go:11` 的 continuity 没有独立沙箱代际约束。

**已复现：** 第一次响应保存完整 snapshot；只使 slot TTL 过期；follow-up 分配第二个沙箱后，顶层 sandbox token 与 snapshot 内 token 不相同，mock 仍接受（`TestPrismReviewExpiredSandboxSnapshotMismatch`）。这是线缆字段不一致的确认；真实上游具体返回码本次未测试。

**具体参考差异：** 参考 `chatgpt-prism2api/.../client.go:875` 明确要求会话与沙箱同批；`oai-prism/internal/facade/sandbox.go:64`–`:75` 也按 token 相同与否区分可保留的同步状态。本仓只检查 project 身份，不足以保护沙箱原生连续性。

**影响：** 默认 sandbox TTL=2m、cache TTL=10m，中间有效 cache handle 可能在正常 follow-up 上变成 restore/lookup 失败，而不是明确的 context_expired；旧 sandbox 失效后尤其明显。

**修复：** continuation 绑定 sandbox generation；轮换时显式拒绝过期原生句柄，或在上游协议允许时完成快照迁移并同步所有相关身份字段。增加 TTL 交叉用例，不只测 project 被替换。

#### M3 — 默认 vision 路径会丢历史图片引用，而且上传成功不证明模型可见

**证据：** `images.go:186` 生成 `input_file`；`translate.go:272`–`:286` 折叠非当前用户消息时只取 `.Text`，彻底丢弃历史 `input_file` 的 Filename/ProjectPath。`client.go:670` 保存的是预处理前的 input；下一轮 `:505` 会再次下载/解码/上传所有旧图片，生成新随机文件名，然后再将历史文件引用丢掉。

**已复现：** inline=false 的图片首轮 + previous_response_id 纯文本 follow-up，共上传两次；第二次 start input 没有任何图片文件引用（`TestPrismReviewHistoricalFileDropped`）。inline=true 可把历史图片变成文字说明保留，不能因此判定默认路径也正确。

**具体参考差异：** `chatgpt-prism2api/.../stream.go:60`–`:67` 优先为当前图片分预算，`:123`–`:127` 为历史附件保留内联内容或标记；本仓默认路径连标记也不保留。参考 `attachments.go:24`–`:33` 解释 headless upload 不进入 Y-Sweet 文件树，不能单靠 project_path 保证可见。

本仓还仅在 `images.go:176` 检查返回 id/fileUuid，统计 sediment/URL 后不用这些引用（`:179`–`:186`）。`DELIVERY.md:57`、`:96` 已承认真实识图靠 inline 还原；默认 `ImageInline=false`（`RUNBOOK.md:74`）未获等价 live 验证。

**影响：** 默认多轮 vision 静默失去上下文；重复上传增加延迟与上游资源。inline 模式又从最早历史依次消耗全链预算（`images.go:139`、`:153`），旧图可使当前新图因累计预算而 413。

**修复：** 保留可访问的历史附件标记/引用；缓存经验证的物化结果，避免每次重传；预算优先当前图片。未实现 Y-Sweet 文件树时，将 headless vision 标明为需 inline 的实验能力，或让不可见的默认路径明确失败。

#### M4 — shared sandbox 的锁等待不可取消，破坏请求时限

**证据：** `sandbox.go:51` 在 `shared.mu.Lock()` 之后才检查 ctx；`:62` 又在锁内执行完整远端 prepare 链，可能等待资源同步至请求超时。`invalidate`（`:24`）也阻塞取同一把锁。槽位已在 `client.go:454` 取得，排队限制此时不再保护该等待。

**已复现：** 持有 shared mutex，传入已经 canceled 的 context，prepare 仍不能返回；释放锁后才返回 Canceled（`TestPrismReviewCanceledSharedPrepareWaitsForMutex`）。

**具体参考差异：** `chatgpt-prism2api/.../prewarm.go:25`–`:47` 专门解释 follower 的阻塞问题，用 DoChan + select ctx.Done 让预算到期立即退出。

**影响：** opt-in shared/prewarm 模式中，一条慢 warmup 可让多个已超时请求继续占 slot，扩大尾延迟；race PASS 检不出这种活性问题。

**修复：** 用可取消的单飞等待/信号量，减少持锁远端 I/O，区分状态发布与等待。让取消者可以及时退出，且不破坏仍运行的 prepare leader。

#### M5 — 工具验证仅到 JSON object；`strict` 静默丢失，报告称 schema 校验过度

**证据：** `translate.go:156`–`:179` 只做类型/名称/重复名检查及缺省参数；`types.go:50` 没有 Strict 字段。`tools.go:45`–`:59` 验证的是工具名称、tool_choice 与 JSON object，不检查 required、属性类型、additionalProperties 等 schema 语义。

**已复现：** 对 `q:string`、required q、additionalProperties=false 的声明，`{"q":42,"extra":true}` 仍被接受为工具调用（`TestPrismReviewSchemaNotEnforced`）。

**影响：** 声明 strict 的调用方得不到约定的 schema 保证，可能把错误参数交给客户端工具。一般非 strict prompt-envelope 工具没有完整 schema 保证不必然是 bug；问题在于 strict 被接受后无声降级，以及 `TEST_REPORT.md:81`–`:85` 声称 schema 校验且门面无明显缺口。`RUNBOOK.md:158` 对“不做 full JSON Schema”反而更诚实。

**参考比较：** `chatgpt-prism2api/.../tools.go:16`–`:24` 明确称工具调用为提示词仿真；本仓采用同类方法，不能把它描述成上游原生严格 schema 通道。

**修复：** 明确拒绝不支持的 strict，或实现严格验证；验证 parameters 的声明形状。同步报告术语为“声明名称 + JSON object 验证”，并说明 prompt 遵从与真正客户端往返另需 live 验证。

#### M6 — 输出提取过窄，合法拒绝/变体可被变成空成功

**证据：** `client.go:335`–`:350` 只保留 role=assistant、type=message 中的 output_text，以及 function_call；`types.go:33` 没有 refusal/summary 字段。`:625` 只要求 Output 非 nil，空数组或完全未识别的 output 可继续到 completed、缓存及 live_verified（`:665`–`:667`）。Reasoning 字段存在于 Result/Event，但没有实际填充、流式输出路径。

**具体参考差异：** `oai-prism/internal/prism/client.go:799`–`:818` 明确处理 reasoning，以及 role 为空、text/input_text/refusal 等内容；`:827` 还有顶层 item.Text fallback。本仓省略这些已被参考解析器考虑的分支。

**影响：** refusal 或参考支持的输出变体可能得到 200 + 空文本，丢失业务含义；只有 reasoning 的过程也无法提供 DESIGN 最初承诺的早期 reasoning/progress（`DESIGN.md:37`）。本次为静态字段路径确认，未声称真实账号此次发送了这些变体。

**修复：** 明确支持的 output contract；保留 refusal 的正确 facade 表示；不支持的业务输出应显式失败，避免将“数组存在”当成可用完成结果。若不提供 reasoning/progress，文档应准确说明。

#### M7 — TEST_REPORT 的总体验收标签超过测试与 live 证据

**证据：** `TEST_REPORT.md:12`–`:16` 把 low latency、tools、vision、cache 的 live 状态一律列为 implemented，`:24` 宣称六项均实现。相比之下，`:84`、`:108`、`:169` 承认没有重跑完整工具/vision matrix，vision 文件树缺口仍存在。`:161` 所谓缓存兼容修复正是 M1。

`feature_coverage_test.go:55`–`:101` 的“TTFPAndTPS”只验证 mock 首个 callback <250ms、rpm/tpm 非零；`metrics.go:29`–`:51` 是最近 60s 请求与**输入+输出** token 求和，不是生成 TPS。mock 返回固定 24/8 token（`mock.go:279`），并主动提供非终态前缀文本（`:248`–`:255`）。这些不能证明真实生成速度、持续容量或逐 token streaming。

**影响：** 若把该表当生产验收，容易忽略 H1–M6 的实际缺口，并将合成生命周期帧、单轮 PONG、单张小 PNG 当完整功能和性能保证。详细历史证据本身有价值，不应丢弃；问题是状态标签与归因强度。

**修复：** 使用“local mock verified / historical live smoke / partial / unsupported”之类可核验状态；性能拆分 transport、meaningful first output、完成耗时、输出速率和成功率。具体对照见第 4 节。

### Low

#### L1 — Close 不等待活动 Run/stop，进程退出可能遗留上游任务

**证据：** `client.go:91` 只将 keepWarm 加入 c.wg，`:95` 的 Close 等待该 worker，不等待 Run；Run 的停止逻辑在 `:567`–`:575` 的 defer。`main.go:425` 将 ClosePrism 放在 main 的 defer；`:659`–`:662` 的 Shutdown 10s 超时后仍继续退出。

**影响：** 长流超过 drain 窗口时，Close 取消 Run 后可在其 detached stop 完成前结束进程。运行中正常 disconnect 已有 stop 测试；不能据此证明退出阶段也排空了上游任务。此项是静态生命周期风险，未执行进程退出 live 实验。

**修复：** 在停止接收新请求后，对 Prism active runs 和 bounded cleanup 单独 drain；在 shutdown 次序中明确调用，而不只靠最终 defer。不得为等待永不退出的写无限阻塞 shutdown。

#### L2 — warm_slots 是状态位计数，不是当前可用/持续预热保证

**证据：** `sandbox.go:90` 在有效期内直接返回；keepWarm 在 `:298`–`:305` 同步占用闲置 slot 做 probe，过期后才重建。`Metrics.WarmSlots` 在 invalidate 时才减（`:30`–`:34`），未在 TTL 到点自动减；`prism_handler.go:195` 直接返回它。probe 成功也不会推迟 s.expires。

**具体参考差异：** `chatgpt-prism2api/.../prewarm.go:9`–`:23` 在 TTL 一半主动换新，并用独立 flight/代际保护让 warm 请求使用仍有效的旧令牌。本仓既未采用这一策略，也没有 expiry-aware warm gauge。

**影响：** `warm_slots=capacity` 不保证下一请求不会付冷链；benchmark 用它作为 ready 条件（`tools/prism/live_perf.py:158`）只能得到近似暖态。周期 probe 同时降低短时间内可用的 idle slots。更换 owner/到期也可能产生额外项目/沙箱；上游 cleanup 目前是人工责任，DELIVERY 已披露。

**修复：** 让健康信息区分分配过、未过期、可取用的 slot；对空闲预热考虑提前刷新与非阻塞发布，配合 M2 的 generation 处理；补充 warmup/probe latency，而不是仅按 warm_slots 判暖态。

#### L3 — RUNBOOK 与新报告有直接相反的运行说明

**证据：** `RUNBOOK.md:135`–`:137` 仍说 live_verified 有意 false、只有 mock；`:190`–`:192` 则正确说成功 live turn 后会变 true。`:208`–`:209` 说 cached_tokens 恒为零，而 `TEST_REPORT.md:161` 和当前 `prism_stream.go:16` 是非零映射。`DESIGN.md:4` 也保留“live calibration pending”的旧顶层状态。

**影响：** 运维无法仅依文档判断 health/usage 应当怎样，且缓存语义冲突容易掩盖 M1。

**修复：** 对齐当前能力与限制；cache 段先按 M1 修正代码语义，再统一文档。保留历史实验时明确加历史标签，避免在同一 runbook 中并列矛盾事实。

### Info

#### I1 — URL 与凭据的信任边界整体清楚，但不是域名 allowlist

**证据与影响：** `config.go:155`–`:175` 允许运营方配置任意 HTTPS origin/provider，拒绝 URL userinfo/query 等，并仅允许 loopback 明文 HTTP；它没有限定为 prism.openai.com。`sandbox.go:38`–`:44` 验证 sandbox 同源，但没有限定 sandbox 路径命名空间。这里的配置/provider 是受信任管理输入，不是下游用户任意 URL，因此本次不把它报为已确认 SSRF 漏洞；“HTTPS”也不能被写成“已做 Prism 域名 allowlisting”。配置错误仍会把 Cookie 发给配置的 origin。

用户控制的 remote image 则走独立防护：`images.go:55`–`:80` 禁代理、校验所有 DNS 地址、直接 dial 校验 IP、限制 HTTPS redirect；`:105`–`:106` 关闭其 idle connections；不携带 Prism Cookie。`:124`–`:129` 约束字节与像素。此部分明显比 `oai-prism/internal/facade/image.go:106`–`:120` 的参考下载器更严格。

可选收紧：为正式上游/provider 建立显式部署 allowlist，并为 sandbox path 做命名空间检查；保留 loopback mock 的明确例外。当前 remote-image 单测主要验证纯 IP 分类及关闭 remote 功能，不覆盖实际 DNS/redirect 重绑定链（`client_test.go:335`–`:348`）。

#### I2 — 本次 race PASS 有效，但不证明协议、活性和资源释放完整

**证据与影响：** baseline race 执行成功；cache 使用 mutex、immutable serialized entries、decode 后副本（`tokencache.go:54`–`:86`）；账号 cooldown 与 metrics 多用 atomic。未检测到数据竞争值得保留。但 H2/M4 是阻塞/活性问题，M2/M3 是协议与上下文问题，race detector 不会把它们标成 race。`mock.go:37` 的 turn 只跟踪 seq/state/active，不绑定 project/conversation/sandbox generation，`:182` 的 start 验证不足以检测这些错配。

## 3. What is solid

1. **核心轮询协议方向正确。** `types.go:11`–`:13`、`client.go:545`、`:715` 使用 `/api/llm/response_with_tools_{start,status,stop}`，start 的 conversationId/previousResponseId 为 camelCase，status/stop 使用 request_id 与 opaque turn_state。与参考 `oai-prism/internal/prism/types.go:243`、`:301` 和 `chatgpt-prism2api/.../endpoints.go:13` 一致。RawMessage 保留状态对象，逐轮更新；没有盲目重发非幂等 start。
2. **HTTP-200 业务失败没有误计成功。** `client.go:368`、`:585` 正确处理嵌套失败，结构化 httpStatus 分类，并按账号共享 401/403/429 cooldown；相关测试验证拒绝后不写 cache。stop 使用独立短时 context、最近状态并在释放 slot 前执行，正常 cancel/disconnect 测试真实检查 mock Active 清零。
3. **本地并发门实际存在。** slots 限制 accounts×concurrency，waiters 数量、排队时间和 start pacing 有边界；覆盖至少 24 个并行任务、shared 10-way，以及饱和/超时。HTTP transport 连接复用配置完整。它们证明本地机制，不证明 upstream 接受额度。
4. **流式 facade 有明确生命周期与失败事件。** Chat role/delta/finish/DONE、Responses item/content/function/completed 路径存在；工具文本完成前缓冲，避免半个 envelope 泄露。proxy 的 flush/keepalive/disconnect 测试比简单字符串快照更有价值。
5. **本地 cache 的所有权和内存约束有实测。** owner+response ID、TTL、LRU count/bytes、immutable copy、并发读写和淘汰计数；static key 用 hash 分开 namespace。store:false 不写。匿名共享 namespace 在 RUNBOOK 中已披露。cached_tokens 语义错误不否定本地 cache 机制本身。
6. **上传线缆格式已纠正。** raw bytes、正确 MIME、Prism 文件/edit-access headers，与 `prism-proxy/src/prism-client.mjs:426`–`:444` 一致；mock 上传端真实用 DecodeConfig 检查字节，不只是接受任意返回码。生成 UUID 文件名与 base64 单引号命令避免把客户端文件名直接拼进 shell。
7. **错误与 provider header 处理克制。** 不返回 upstream body/Set-Cookie/snapshot；provider header 有白名单和 CR/LF/NUL 检查，不跟随上游/provider redirect；每次 upstream call 重新请求票据，不缓存一用 ticket。配置 JSON 排除 material bearer 和 credential 列表。
8. **DELIVERY 对容量失败相对诚实。** 明确最终 4/10、单轮 OCR 的 inline 条件、成功样本 percentile、chars/4 TPS 以及没有持续吞吐保证。历史日志保留失败迭代和控制组，不是只列成功结果。

## 4. What is overclaimed

| 声明/目标 | 本次可以支持的结论 | 缺口与证据 |
|---|---|---|
| 六项功能全部 implemented（TEST_REPORT:24） | 六项都有相关代码及命名单测 | H1–M6 显示实质边界错误；命名覆盖不能代替正确性验收 |
| live low latency implemented（:68） | 历史暖态 PONG meaningful TTFP 6.92s / 最近 smoke 7.2925s | 无 SLA、重复样本控制或持续压测；mock 5ms 是人为轮询延迟，不是模型性能 |
| TPSAndTTFP 测试 | mock callback 延迟及计数器存在 | rpm/tpm>0 不是输出 TPS；tpm 含输入 token；没有测试吞吐回归界限或长时间稳态 |
| stream implemented | 真实 HTTP SSE facade、生命周期与 keepalive 可以工作 | 参考 `oai-prism/.../client.go:748` 将 response 描述为终态才有；如果 upstream 不给 pending text，本仓第一次有意义输出只能等完成。工具声明存在时也明确整体缓冲（client.go:607、:638） |
| tools live implemented、无实质 facade gap（:80、:84） | 初期历史记录有工具调用输出，mock 验证调用/结果文字和 ID 可回传 | v2 live probe 没有工具分支；ToolsRoundTrip（client_test.go:87–114）只验证 sunny/ID 出现在 input，mock 下一轮仍产出固定调用，不验证模型正确消费结果完成任务。无完整 live 多工具/forced/parallel/strict matrix |
| vision live implemented（:104） | 历史一张 480×240、1,811-byte PNG 在 inline+low effort 下 OCR/颜色识别成功 | meaningful TTFP=49.7399s；默认 upload-only 不等价；历史图片、常见大图、JPEG/GIF/WebP 的 live 识图没有同等证据。Shape 判断仅查 red/blue 字符（live_perf.py:143–144）不能全面证明形状准确 |
| cache live implemented（:116、:161） | 历史 native continuity 三轮 recall + 本地 read/write counters 有证据 | 新 CacheWriteTokens/API cached_tokens wiring 的 live 断言未出现在仅 PONG 的 smoke；标准 cached_tokens 更是 M1 的语义回归。旧二进制成功不能自动验证新源码字段 |
| 10-way mostly succeed（GOAL-PERF:33、:79） | 本地 10-way 可入场，历史实际完成 4/10 | 60% 错误，目标未达；日志 peak_inflight 还包括 paced 等待，不能单凭它证明 10 个 upstream generation 同时执行 |
| proven hard upstream limit / 无本地修复余地 | 403 响应确实由 upstream 返回，降并发/串行/刷新等控制后仍有拒绝 | 不能区分账号策略、票据接受、metadata/context 或本地请求兼容缺陷。相同 generic 403 不能证明精确 quota，更不能排除 H3/M2；DELIVERY:94–95 已承认票据不同/未过期不证明请求被接受 |
| production-grade（GOAL:4） | 有可工作的实验通道、文档与 mock 回归 | key 策略、可取消等待/写入、沙箱迁移、默认 vision 和输出保真仍需修复。合理结论是“实验通道已验证部分路径” |

### 历史 live 证据核对

以下值来自现存日志白名单提取，本次没有重新调用服务：

- `logs/prism-live-e2e-evidence-v2.txt:43`：暖态 PONG completed=true，TTFP=6.92s。
- 同文件 `:45`：raw upload + inline + low effort PNG completed=true，OCR/颜色检查 true，TTFP=49.7399s。
- 同文件 `:46`–`:48`：最终二进制三轮 Responses completed=true，recall 检查 true；前面多轮失败记录仍保留。
- `logs/prism-perf-10conc.txt:11`：最终批次 4 完成 / 6 错误，成功样本 TTFP p50=9.9287s、p95=12.5654s，rough aggregate TPS=58.352，peak local inflight=10。这四条成功流每条记录 4 个 data event；结合 Chat role、finish、usage 路径，不能由这些样本宣称细粒度 token 流。
- `logs/prism-live-smoke-feature.txt:1`：completed=true，TTFP=7.2925s，first_frame=0.0185s；因此 TEST_REPORT:96 的“通常 <10ms”至少不能代表这一次样本。

`tools/prism/live_perf.py:184`–`:196` 使用成功流的 TTFP 分位，吞吐为成功输出字符/4/整批墙钟时间；算法与 DELIVERY 的限定说明一致。应始终把 4/10 成功率与分位同时显示。仅 4 个成功样本的 p95 是样本最大值，不是可靠容量分布。

## 5. Concrete fix list

| 优先级 | 动作 | 验收条件 |
|---|---|---|
| P0 | 修 H1：为 Prism 增加明确授权边界；未支持的 group/plan/scope key fail closed | 受限 key 不触发 upstream start；合法映射 key 正常运行；scope usage/并发有对应账号身份 |
| P0 | 修 H2：逐次写 deadline、取消 upstream、可退出心跳 | 慢读客户端不超过明确写预算占用 slot；ctx canceled 后有界释放；正常长流不中断 |
| P1 | 修 H3：保留完整 material 的 user/context identity，明确无材料 UserID 来源 | provider 字段不被空值覆盖；身份不一致 fail closed；两种 material 模式分别覆盖 |
| P1 | 修 M1：分离本地估算与标准 cached_tokens，修正新测试中的错误预期 | 标准 cached_tokens 仅来自确证 provider 值；本地 R/W counters 仍可见；不出现 cached>input |
| P1 | 修 M2：continuity 绑定 sandbox generation，定义轮换/迁移行为 | TTL/invalidated sandbox 后不会混合新旧 token；返回明确 context_expired 或完成合法恢复 |
| P1 | 修 M3：保存历史附件引用/物化状态，避免重复上传，当前图片优先预算 | 图片首轮→纯文本 follow-up 不丢图；旧图片不抢占新图片全部预算；默认 headless 能力有明确边界 |
| P1 | 修 M4：共享准备过程用可取消等待与安全状态发布 | 等锁请求 deadline/cancel 能及时退出；取消 follower 不破坏 leader；owner 切换与并发覆盖 |
| P2 | 修 M5：明确 strict 支持或拒绝，验证 schema 声明与必要语义 | strict 不无声降级；错误参数不会被当严格有效调用交给客户端 |
| P2 | 修 M6：输出分支保真、拒绝类型、未知/空终态策略 | refusal/参考支持变体得到正确 facade 或明确错误；无文本有效输出不误记成功 |
| P2 | 补 L1 的 shutdown drain 和 L2 的 expiry-aware warm 状态 | 活动 Run/stop 有界 drain；warm gauge 不把过期 slot 当准备完成 |
| P2 | 重写 TEST_REPORT/RUNBOOK/DESIGN 当前状态，绑定源码/构建版本与测试条件 | 保留失败日志；matrix 区分 mock、历史 smoke、partial；删除 schema/cache/性能错误结论 |
| P3 | 增加性能观测及持续试验：排队、prepare、provider、start、poll、首个输出、完成 | 成功率、失败率、冷暖与输出速率一起报告；单轮 PONG 不用来代表 tools/vision 延迟 |

所有上述都是后续建议，本次未改实现。真实材料恢复、live 验证与部署须按后续任务授权执行，不能从这份只读审阅推导为当前已验证。

## 6. Suggested follow-up tests

1. **授权集成：** 使用真实测试 DB 的已认证 key，覆盖 AllowedGroupIDs、PlanAllow、scope skip/reject、scope MaxConcurrency；不要只构造零依赖 Handler。
2. **流式活性：** 可控阻塞 Writer + 真实慢读 HTTP 客户端；取消/写超时后验证 slot、key permit、心跳 goroutine 与 upstream active 全部有界归零。
3. **material：** 完整 captured metadata 带非空 userId 和完整 snapshot；token-only、provider-only、headers-only 分别测试；对身份错配、过期材料、空 Cookie fail closed。
4. **连续性：** 每轮改变 upstream response ID、last_turn_id、cursor；验证最近字段而不是永久固定 mock 值。加入 sandbox TTL<cache TTL、keepwarm 轮换、取消后续接、project replacement、跨模型/分支续接的明确策略。
5. **vision：** 默认与 inline 模式各测历史图→新文字、新图→旧图、image-only、budget 边界；确认实际 view_image 输入和任务结果。remote-image 覆盖 HTTPS redirect→内网、混合 DNS 结果、DNS rebinding 与超限响应。
6. **tools：** 验证严格 schema 的 required/type/additionalProperties，forced/none/parallel、重复 call ID、native+envelope 混合；round-trip 第二轮应根据工具返回数据产生最终答案，而不只在 input 字符串中查找结果。
7. **SSE contract：** 解析完整事件，断言顺序、sequence、item_id/output_index、arguments 拼接与 terminal 唯一；覆盖非单调 text、截断、不完整 payload、只有 refusal/unknown output、stop/provider 超时。
8. **usage：** 本地 cache token 与真实 provider cached tokens 分离；上游缺失/部分 usage、负值、图片链、缓存 write skipped、失败读命中后的计数语义；核对 API、metrics、DB 三者。
9. **warm/shutdown：** 慢同步 leader 下 canceled follower、shared owner 并发切换、background invalidation、过期 gauge、Close 与 Run 的时序；race 用来补数据竞争验证，另加活性断言。
10. **live/性能：** 修好协议与策略后再用合法 operator 材料重跑工具多轮、默认 vision、较大常见图和 store/cache 字段；重复冷暖与持续 10-way，保留全部错误。区分 first frame、first meaningful output、输出持续时间与模型 tokenizer/billing 计数。

## Redacted executive summary

现有 28 个 channel 测试、5 个 Prism proxy 测试及同范围 race 通过，核心轮询、取消、owner cache 和错误脱敏有扎实基础。但已复现授权边界遗漏、阻塞写无法取消、browser identity 被覆盖、沙箱快照错配和历史图像引用丢失；标准 cached_tokens 也存在明确语义回归。历史 live 只支持部分路径，最终 10-way 为 4/10，不能视为生产容量验收通过。优先修复 High 项并纠正缓存/报告语义。本报告不包含真实凭据，未修改生产代码或参考树。
