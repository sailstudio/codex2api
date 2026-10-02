# Prism Feature Test Report / Prism 功能测试报告

> Historical feature evidence from the earlier 2026-10-02 run. Current mock/race fixes and remaining gaps are recorded in [CODEX_FIXES.md](CODEX_FIXES.md). No live checks, service restarts, or production health checks were repeated for the fixes. Historical live successes do not validate the current source revision.

**Generated / 生成时间:** 2026-10-02 07:55:10 CST (Asia/Shanghai)  
**Workspace:** `/workspace/codex2api-run`  
**Scope / 范围:** Requirements 1–6 (concurrency, TTFP/TPS, tools, streaming, vision, token-cache R/W)

## Executive summary / 摘要

| # | Requirement / 需求 | Code status / 代码 | Live / 线上 | Explicit feature test / 显式用例 |
|---|--------------------|--------------------|-------------|----------------------------------|
| 1 | High concurrency / 高并发 | local mock verified | historical partial (4/10) | `TestPrismFeatureHighConcurrency` |
| 2 | Low latency (TTFP/TPS) / 低延迟（TTFP/TPS） | local mock verified | historical smoke / partial | `TestPrismFeatureLowLatencyTTFPAndTPS` |
| 3 | Tool calling / 工具调用 | local mock verified | historical smoke / partial | `TestPrismFeatureToolCalling` |
| 4 | Streaming responses / 流式响应 | local mock verified | historical smoke / partial | `TestPrismFeatureStreamingResponses` |
| 5 | Image/vision parsing / 图像/视觉解析 | local mock verified | historical smoke / partial | `TestPrismFeatureImageVisionParsing` |
| 6 | Read/write token cache counting / 读写 token 缓存计数 | local mock verified | historical smoke / partial | `TestPrismFeatureTokenCacheReadWriteCounting` |

**Automated results / 自动化结果 (this run):**
- `internal/prismchannel`: **28** top-level tests, **43** including subtests — **PASS** (0 fail)
- `proxy` (`-run 'Prism|prism'`): **5** top-level tests — **PASS** (0 fail)
- `-race`: both packages **PASS**
- Live short smoke (:8081): **PASS** — PONG, TTFP **7.2925s**, first_frame **0.0185s**; stock :8080 healthy

**Verdict / 结论:** Six areas have local mock coverage, with historical live smoke evidence for selected paths. This is an experimental channel with partial acceptance. The historical 10-way result was 4/10 successes; generic upstream 403 responses do not establish an exact capacity limit or exclude local compatibility problems. The latency test checks mock callback timing and counters; TPM includes input and output and is not generation TPS. Strict tools are unsupported. Default upload-only vision visibility remains unverified.

## Commands / 命令

```bash
# Feature-focused unit/mock suite
GIN_MODE=release go test ./internal/prismchannel/... -run 'Prism|prism|Feature' -count=1 -v
GIN_MODE=release go test ./proxy/ -run 'Prism|prism' -count=1 -v

# Race (reasonable scope)
GIN_MODE=release go test ./internal/prismchannel/... -run 'Prism|prism|Feature' -count=1 -race
GIN_MODE=release go test ./proxy/ -run 'Prism|prism' -count=1 -race

# Optional live short smoke (secrets local; never printed)
# writes logs/prism-live-smoke-feature.txt
```

Artifacts:
- `logs/prism-feature-test-verbose.txt` / `logs/prism-proxy-test-verbose.txt`
- `logs/prism-feature-race.txt`
- `logs/prism-feature-test.jsonl`
- `logs/prism-live-smoke-feature.txt`
- `logs/prism-feature-coverage.json` (machine-readable twin of this matrix)
- Historical: `logs/prism-perf-10conc.txt`, `logs/prism-live-e2e-evidence-v2.txt`

## Requirement matrix detail / 需求矩阵明细

### 1. High concurrency / 高并发

| Field | Value |
|-------|-------|
| Status (code) | local mock verified |
| Status (live) | historical partial (4/10); not rerun |
| Implementation EN | Per-account slot pool, admission queue, shared sandbox pool, prewarm, paced RPM admission, cooldown skip. |
| 实现说明 ZH | 按账号槽位池、准入队列、共享沙箱池、预热、RPM pacing、冷却跳过。 |
| Tests | `TestPrismFeatureHighConcurrency`, `TestPrismConcurrencyAndAdmission`, `TestPrismSharedSandboxPrewarmAndOwnerIsolation`, `TestPrismPacedAdmissionProtectsUpstreamWindow` |
| Gaps EN | Live 10-concurrent streams remain ~4/10 complete with upstream HTTP 403 denials on available account material (see logs/prism-perf-10conc.txt final run). |
| 剩余缺口 ZH | 线上 10 并发仍约 4/10 成功，剩余为上游 HTTP 403（见 logs/prism-perf-10conc.txt 最终跑）。 |

### 2. Low latency (TTFP/TPS) / 低延迟（TTFP/TPS）

| Field | Value |
|-------|-------|
| Status (code) | local mock verified |
| Status (live) | historical smoke / partial; not rerun |
| Implementation EN | TTFT metrics on first streamed delta; rolling rpm/tpm gauges; poll backoff; keepalive; warm slots/prewarm. Mock TTFP ~5ms; live warm PONG TTFP ~7.3s (improved vs ~20s baseline). |
| 实现说明 ZH | 首包增量记录 TTFT；滚动 rpm/tpm；轮询退避；keepalive；预热槽。Mock TTFP ~5ms；线上暖路径 PONG TTFP ~7.3s（相对 ~20s 基线改善）。 |
| Tests | `TestPrismFeatureLowLatencyTTFPAndTPS`, `TestPrismProtocolStreamCache`, `TestPrismStreamingFlushKeepaliveAndDisconnect` |
| Gaps EN | Live meaningful TTFP still dominated by upstream Prism generation (~7–13s). Aggregate TPS is approximate chars/4/wall, not billing tokens. |
| 剩余缺口 ZH | 线上有效 TTFP 仍主要由上游生成耗时决定（约 7–13s）。聚合 TPS 为字符/4/墙钟近似，非计费 token。 |

### 3. Tool calling / 工具调用

| Field | Value |
|-------|-------|
| Status (code) | local mock verified |
| Status (live) | historical smoke / partial; not rerun |
| Implementation EN | Chat/Responses declaration shape, tool name and JSON object validation; strict:true rejected; <tool_call> envelope + native function_call parse; client-side tool results folded into history. |
| 实现说明 ZH | Chat/Responses 工具声明形状、名称与 JSON object 校验；strict:true 明确拒绝；信封与原生 function_call 解析；客户端工具结果回填历史。 |
| Tests | `TestPrismFeatureToolCalling`, `TestPrismToolsRoundTrip`, `TestPrismToolValidation`, `TestPrismMockSmokeStreamToolsVisionCache` |
| Gaps EN | Full JSON Schema argument validation is unsupported; strict:true is rejected. Full live tool round-trip/forced/parallel matrix was not re-probed. |
| 剩余缺口 ZH | 不支持完整 JSON Schema 参数校验，strict:true 明确拒绝；未重跑完整线上工具往返、强制选择和并行用例。 |

### 4. Streaming responses / 流式响应

| Field | Value |
|-------|-------|
| Status (code) | local mock verified |
| Status (live) | historical smoke / partial; not rerun |
| Implementation EN | Polling upstream mapped to OpenAI SSE (chat + responses); early response.created; keepalive comments; disconnect stop; business failure event. |
| 实现说明 ZH | 上游轮询映射为 OpenAI SSE（chat + responses）；提前 response.created；keepalive；断连 stop；业务失败事件。 |
| Tests | `TestPrismFeatureStreamingResponses`, `TestPrismProtocolStreamCache`, `TestPrismStreamingFlushKeepaliveAndDisconnect`, `TestPrismStoreFalseAndStreamFailure`, `TestPrismMockSmokeStreamToolsVisionCache` |
| Gaps EN | Mock SSE lifecycle/deltas are covered. Historical smoke first frame was 18.5ms; meaningful output arrived at 7.2925s. No live latency or token streaming guarantee. |
| 剩余缺口 ZH | Mock 覆盖 SSE 生命周期与增量；历史 smoke 首帧 18.5ms，有效输出 7.2925s，不代表线上延迟或逐 token 流保证。 |

### 5. Image/vision parsing / 图像/视觉解析

| Field | Value |
|-------|-------|
| Status (code) | local mock verified |
| Status (live) | historical smoke / partial; not rerun |
| Implementation EN | Data-URL PNG validation, raw-byte Prism upload, storage-reference path, optional inline view_image restoration; SSRF/size bounds. |
| 实现说明 ZH | Data-URL PNG 校验、原始字节上传、存储引用路径、可选 inline view_image；SSRF/大小限制。 |
| Tests | `TestPrismFeatureImageVisionParsing`, `TestPrismImageUploadAndParse`, `TestPrismImageInlineAfterValidatedUpload`, `TestPrismMockSmokeStreamToolsVisionCache` |
| Gaps EN | Browser/Y-Sweet file-tree integration remains out of scope. Prior live OCR evidence ORBIT 739 retained in DELIVERY.md. |
| 剩余缺口 ZH | 浏览器/Y-Sweet 文件树集成仍不在范围内。历史线上 OCR 证据 ORBIT 739 见 DELIVERY.md。 |

### 6. Read/write token cache counting / 读写 token 缓存计数

| Field | Value |
|-------|-------|
| Status (code) | local mock verified |
| Status (live) | historical smoke / partial; not rerun |
| Implementation EN | Owner-scoped LRU TokenCache with CacheRead/Write Tokens+Bytes metrics; Result.CacheReadTokens + CacheWriteTokens; usage exposes prism_cache_* local estimates with prism_cache_estimated:true; standard cached_tokens comes only from confirmed provider input_tokens_details (otherwise 0). |
| 实现说明 ZH | 按 owner 的 LRU TokenCache，读写 Tokens/Bytes 指标；Result 含 CacheRead/WriteTokens；usage 暴露 prism_cache_* 本地估算与 prism_cache_estimated:true；标准 cached_tokens 只取上游确认的 input_tokens_details，缺失为 0。 |
| Tests | `TestPrismFeatureTokenCacheReadWriteCounting`, `TestPrismProtocolStreamCache`, `TestPrismCacheBoundsExpiryAndCopies`, `TestPrismCacheConcurrentOwnersAndEviction`, `TestPrismMockSmokeStreamToolsVisionCache` |
| Gaps EN | Token estimates are UTF-8 byte/4 approximates, not billing. Cross-owner previous_response_id correctly rejected. |
| 剩余缺口 ZH | Token 为 UTF-8 字节/4 近似，非计费。跨 owner 的 previous_response_id 正确拒绝。 |

## Latency / concurrency numbers / 延迟与并发数据

### Mock (this run)

| Metric | Value |
|--------|-------|
| Mock TTFP (`TestPrismFeatureLowLatencyTTFPAndTPS`) | ~5–6 ms |
| Mock recorded TTFT metric | ~5–6 ms |
| Mock TPS inputs | rpm=1, tpm=32, total_tokens=32 (single turn) |
| Mock concurrency (`TestPrismFeatureHighConcurrency`) | 24 parallel runs admitted under capacity=8, max_active ≤ 8 |

### Live short smoke (2026-10-02 07:54:34 CST)

| Metric | Value |
|--------|-------|
| Endpoint | `POST /v1/chat/completions` stream PONG on :8081 |
| HTTP | 200 |
| Completed | true (text `PONG`) |
| first_frame_s | 0.0185 |
| ttfp_s (meaningful) | **7.2925** |
| warm_slots | 10/10 before and after |
| live_verified | true |
| Stock :8080 `/health` | status=ok, available=1 |

### Live historical 10-conc (from `logs/prism-perf-10conc.txt`, label `final-refreshed-session-10conc-warm`)

| Metric | Value |
|--------|-------|
| completed / errors | **4 / 6** |
| ttfp_p50_s / ttfp_p95_s | 9.93 / 12.57 |
| rough_aggregate_tps | 58.35 |
| peak_inflight | 10 |
| Dominant errors | `generation_failed_http_403` |

Warm standalone historical PONG TTFP improved from ~20.01s baseline to ~6.92s (DELIVERY.md); this refresh measured **7.29s**.

## Historical implementation changes (superseded semantics noted) / 历史实现变更

1. **Historical cached_tokens change (superseded):** mapping `Result.CacheReadTokens` into standard cached_tokens was incorrect (M1). It has been removed: local R/W estimates are prism_* fields; standard cached_tokens is provider-confirmed or 0. See CODEX_FIXES.md.
2. **`Result.CacheWriteTokens`** populated on successful store; usage JSON includes `prism_cache_write_tokens`.
3. Added six explicit `TestPrismFeature*` tests mapping 1:1 to the requirement matrix.

## Remaining gaps / 剩余缺口

1. **Live 10-way success** not achieved with current account/verification material (upstream 403). Local admission/concurrency code is covered and green.
2. Local cache R/W counts and fallback usage are approximate (UTF-8 bytes/4). Confirmed provider totals/cache counts remain separate; local reuse does not establish billing savings.
3. Full live tool + vision matrix not re-executed in this short smoke (prior evidence retained).
4. No secrets printed or staged; `.secrets/` remains mode-restricted and untracked for this report.

## Historical pass/fail checklist (not rerun during fixes) / 历史通过清单

| Check | Result |
|-------|--------|
| `go test ./internal/prismchannel/... -run 'Prism\|prism\|Feature' -count=1` | PASS |
| `go test ./proxy/ -run 'Prism\|prism' -count=1` | PASS |
| `-race` (same scopes) | PASS |
| Explicit coverage for all 6 requirements | PASS |
| Live short smoke redacted | PASS |
| Stock :8080 health | PASS |
| Zero secrets in this report | PASS |

---
*Report path: `docs/prism/TEST_REPORT.md`*
