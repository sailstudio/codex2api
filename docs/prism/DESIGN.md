# Prism Channel Design Note (codex2api)

> Date: 2026-10-01 (Asia/Shanghai)
> Status: experimental; local mock verified, historical live smoke partial. Current fixes and open gaps: [CODEX_FIXES.md](CODEX_FIXES.md).
> References: `/workspace/codex-prism` (oai-prism, chatgpt-prism2api, prism-proxy, free-astra, prism-ai-gateway)

## 1. Problem

Stock `codex2api` already proxies Codex / Claude / Grok / Antigravity with high
concurrency, SSE streaming, tools, images, and response-context caches. It does
**not** speak OpenAI Prism (`prism.openai.com`) — the sandbox agent protocol
surveyed under `/workspace/codex-prism`.

We add a **Prism channel** as a first-class upstream, synthesized from the best
ideas in that survey, without rewriting the existing Codex path.

## 2. Protocol reality (calibrated)

Prism is **not** a transparent reverse proxy:

1. Cookie / session JWT auth (`prism_session_token` + `prism_oai_access_token`)
2. Project create / reuse
3. Sandbox warmup: `backend/1/new` → resources-token → Y-Sweet → wait-for-sync
4. `POST /api/llm/response_with_tools_start` (path is **llm**, not lim)
5. Poll `response_with_tools_status` with opaque `turn_state` until completed
6. Optional `response_with_tools_stop` on client cancel (avoid burning quota)

Upstream returns full turns, not true token SSE. Local facade synthesizes stream.

## 3. Best-practice synthesis

| Concern | Adopt from | Decision |
|---|---|---|
| Correct protocol + stop | oai-prism 协议校准 | Use `/api/llm/*`, always echo `turn_state`, call stop on cancel |
| Production Go gateway | chatgpt-prism2api | Adapter package + mock upstream + TTFB phases |
| High concurrency | CAPACITY-PLAN / oai-prism | Per-account semaphore, sandbox warm pool, admission gate, metrics |
| Pseudo-streaming | oai-prism prefix-diff + FaFengFei progress | Emit lifecycle/keepalive then prefix-diff text deltas; reasoning/progress unsupported |
| Client tools | oai-prism toolbridge / Seventy73 envelope | Prompt-envelope + parse `<tool_call>` / codex-exec fences → OpenAI tool_calls |
| Images / vision | oai-prism image.go | base64/URL → project-files upload → `input_file` |
| Token cache R/W count | extend codex2api response_cache patterns | Owner-scoped KV; metrics for hits/misses/bytes/tokens in+out |
| Passthrough | oai-prism `/prism/*` | Optional whitelist raw proxy (strip Set-Cookie) |
| Codex-only front door | free-astra | Model alias `prism-*` routes here; other models unchanged |

## 4. Architecture (patch shape)

```
proxy/
  prism_handler.go          # /v1 routing when model is prism-* OR PRISM_FORCE
  prism_stream.go           # SSE synthesis (chat + responses)
  prism_tools.go            # tool envelope encode/decode
  prism_images.go           # vision preprocess
  prism_metrics.go          # RPM/TPM + cache counters
internal/prismchannel/
  client.go                 # start/status/stop HTTP
  sandbox.go                # warmup + idle keepwarm
  session.go                # cookie principal + refresh hooks
  tokencache.go             # R/W token/context cache + atomic counters
  mock.go                   # in-process mock upstream for tests
  types.go
docs/prism/
  DESIGN.md                 # this file
  RUNBOOK.md                # run / deploy / smoke
```

Integration points in stock code (minimal):

- `RegisterRoutes`: already has `/v1/chat/completions` + `/v1/responses`; Prism
  branches **inside** those handlers when model matches `prism-*` / config.
- New health fields under `/health` or `/health/prism`.
- Config via env (`PRISM_*`) in `config.Config` — no secrets in git.
- `.gitignore` already excludes `.env`, `.api_key`, `.admin_secret`, bins, pids.

## 5. Feature checklist

1. **High concurrency / low latency**: connection pool, sandbox warm set, per-account
   concurrency, admission semaphore, TTFT-oriented poll backoff (short then longer).
2. **Tools**: client tool schemas → prompt envelope; parse model tool fences →
   OpenAI `tool_calls` / Responses `function_call`; round-trip tool results as text.
3. **Streaming**: synthetic SSE (chat completions + responses events); keepalive;
   cancel → upstream stop.
4. **Image parsing**: detect `image_url` / `input_image`; upload; rewrite to
   `input_file`; mock path accepts data URLs without network.
5. **Token cache R/W counting**: cache previous_response / conversation context;
   expose `prism_cache_hits`, `prism_cache_misses`, `prism_cache_read_tokens`,
   `prism_cache_write_tokens`, `prism_prompt_tokens`, `prism_completion_tokens`.
6. **Production**: unit tests + mock e2e, health, metrics text, RUNBOOK, config example.

## 6. Non-goals / safety

- Do **not** invent or commit Prism cookies / JWT / admin secrets.
- Live Prism calls only when operator supplies credentials via env/file outside git.
- Default CI / smoke uses **mock upstream**.
- ToS risk of automating Prism Web remains operator responsibility (document it).

## 7. Success criteria

- `go test ./internal/prismchannel/... ./proxy/ -run Prism -count=1` green
- Service builds; `/health` ok; prism mock smoke: stream + tools + cache counters
- Existing Codex path on `:8080` still healthy
- No secrets in commits

## 8. Delivered implementation and reference calibration

The implementation uses `internal/prismchannel/{client,sandbox,session,types,
translate,tools,images,tokencache,metrics,mock}.go` and two facade files:
`proxy/prism_handler.go` and `proxy/prism_stream.go`. Tools/images/metrics are
kept in the channel package rather than split into additional proxy wrappers.
The stock handlers branch immediately after their existing body read/capture;
authentication, prompt inspection, key model/rate policies, key concurrency, and
usage logs remain in the request path. Keys with group, plan, no-affinity-group,
or scope restrictions fail closed because Prism credentials lack stock account mapping. `config.Config.Prism` loads validated `PRISM_*`
environment settings. Shutdown closes the channel's background worker and HTTP pool.

Reference source review covered:

- `oai-prism/internal/prism/{client,types,extract}.go`: exact llm paths,
  nested business errors, opaque state, sandbox resource/Y-Sweet handoff.
- `oai-prism/internal/facade/{runner,sandbox,image,toolcall,toolbridge}.go`:
  project isolation, slot reuse, image uploads, client execution contracts.
- `chatgpt-prism2api/internal/adapter/prism/{client,endpoints,stream,prewarm,
  tools,attachments,session,sentinel}.go` and `docs/CAPACITY-PLAN.md`:
  bounded admission, conservative sandbox lifetime, warm-path latency, account
  cooldowns, and current browser material requirements.
- `prism-proxy/src/prism-client.mjs`: project/upload/header conventions.
  No reference secret directory or credentials file was read or copied.

Later reference revisions report one-use `openai-sentinel-token` checks on the
API surface and start metadata bound to a browser project/sandbox context. A
cookie-only implementation cannot claim live compatibility on those revisions.
`session.go` therefore supports an optional operator-owned material provider:
it supplies fresh per-request headers and an already synchronized, owner/slot
scoped browser context. All metadata fields are preserved; request model/effort
are overlaid. No token mint, browser login, or credential fabrication is included.
The provider contract and live requirements are in RUNBOOK.md.

Capacity is bounded by accounts × per-account slots (maximum 4096); each active
turn exclusively owns its slot. Queue length and wait duration are bounded.
Different owners get different projects/material contexts, and canceled turns
stop before releasing the slot. Account 429/auth cooldowns are shared by all
slots on that account. Keepwarm touches used idle slots and refreshes expired
contexts; it does not reserve unused slots or run a browser login at startup.

SSE is synthetic: lifecycle/role events flush after admission, heartbeat comments
continue while upstream HTTP calls block, and poll snapshots become prefix deltas.
Text rewrites fail explicitly after visible output. With client tools, text is
buffered through completion so partial envelopes cannot escape. Standard function
tools and JSON tool envelopes/fences are supported; Codex `additional_tools` and
JavaScript/custom-tool bridges are outside this patch.

The process-local cache stores normalized full conversation items with owner
isolation, TTL, LRU count/byte limits, and independent read/write token/byte
counters. These token counts use `ceil(UTF-8 JSON bytes / 4)` and are approximate.
Upstream usage is preferred for request totals; missing usage is labeled estimated.
Local context reuse is never reported as a provider billing cache hit. Cache misses
on `previous_response_id` fail closed; sandbox generation changes also expire native
continuations. Materialized historical attachment references are retained. Upload-only
vision remains experimental and unverified for headless visibility. `store:false`
suppresses new writes. Strict tools and refusal/reasoning-only output are unsupported
and fail explicitly; standard cached_tokens uses confirmed provider counts or zero.

The channel does not change the management UI/channel enum or model catalog.
Operators call `prism-<upstream-model>` directly using an auto-channel key. Raw
passthrough, distributed cache, model discovery, and live capacity guarantees are
not part of this additive patch. DELIVERY.md records verification and gaps.
