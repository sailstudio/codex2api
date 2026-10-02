# Prism channel runbook

The Prism channel is disabled by default. It accepts OpenAI Chat Completions and
Responses requests whose model is `prism-<upstream-model>`. The prefix is stripped
only on the upstream request. `PRISM_FORCE=true` routes all models on those two
HTTP handlers through Prism. Other APIs and model discovery retain stock behavior.

## Verify without credentials

Run from `/workspace/codex2api-run`:

```bash
go test ./internal/prismchannel/... -count=1
GIN_MODE=release go test ./proxy/ -run 'Prism|prism' -count=1
GIN_MODE=release go test ./proxy/ -run TestPrismMockSmokeStreamToolsVisionCache -count=1 -v
GIN_MODE=release go test -race ./internal/prismchannel/... ./proxy/ -run 'Prism|prism' -count=1
go vet ./internal/prismchannel/... ./proxy/ ./config/
go test ./...
mkdir -p .deploy/prism
go build -o .deploy/prism/codex2api-prism .
go test ./internal/prismchannel/ -run '^$' -bench BenchmarkPrismWarmTurn -benchtime=2s -benchmem
curl -fsS http://127.0.0.1:8080/health
git diff --check
git diff --cached --name-only
```

Tests create upstream, material-provider, and authenticated facade HTTP servers on
ephemeral loopback ports. `MockUpstream` checks resource/Y-Sweet sync, metadata,
latest opaque state, image upload, and bounded concurrency. The facade smoke covers
Chat/Responses SSE, standard function tools, vision, cache counters, auth, channel
restrictions, stream failures, early flushing, heartbeat during blocked polling,
and disconnect-to-stop propagation. Fixture tokens are marked `mock-*` and cannot
authenticate to Prism. No external calls or live cookies are needed.

`:8080` is the existing stock service. Verification did not restart or replace it.
The experimental executable is ignored under `.deploy/prism/`. In-process smoke verifies protocol behavior; live acceptance additionally requires
the isolated process on `:8081`. If testing a live
binary, bind `CODEX_BIND=127.0.0.1` and `CODEX_PORT=8081`, with separate database and
cache namespaces. Do not run another binary against the stock service's mutable
database/cache as a substitute for isolation. Use an operator-owned environment
file or supervisor; the existing startup logic loads `.env` by default.

## Environment

All durations use Go syntax, such as `100ms`, `10s`, or `2m`; environment duration
values must be between `1ms` and `24h`. Enabled configurations fail startup on
invalid ranges or missing credentials/material provider. `.env.example` contains
commented options without credentials.

| Variable | Default | Meaning |
|---|---|---|
| `PRISM_ENABLED` | `false` | Enable the optional channel |
| `PRISM_FORCE` | `false` | Route both HTTP completion APIs regardless of prefix |
| `PRISM_BASE_URL` | `https://prism.openai.com` | Upstream origin; HTTP allowed only on loopback for mocks |
| `PRISM_USER_ID` | unset | Required browser user identity for token-only/headers-only contexts; accounts-file equivalent is `user_id` |
| `PRISM_SESSION_TOKEN` | unset | Operator-supplied `prism_session_token` |
| `PRISM_ACCESS_TOKEN` | unset | Operator-supplied `prism_oai_access_token` |
| `PRISM_ACCOUNTS_FILE` | unset | JSON account array; overrides token and user identity environment variables |
| `PRISM_ACCOUNT_CONCURRENCY` | `4` | Logical turn slots per account, range 1–256; total slots ≤4096 |
| `PRISM_PREWARM_SLOTS` | `0` | Startup prewarm budget, capped at capacity; two workers. Zero disables startup allocation |
| `PRISM_SANDBOX_POOL_SIZE` | `0` | Optional shared physical contexts per account, 1–account concurrency; zero gives each slot its own context. Prefer single-owner deployments for sharing |
| `PRISM_ACCOUNT_RPM` | `0` | Optional rolling start-attempt budget per account, 0–4096; zero disables pacing. Uses a 63s guarded minute; waiting is bounded by queue/request deadlines |
| `PRISM_MAX_WAITERS` | `64` | Maximum requests waiting for slots |
| `PRISM_QUEUE_TIMEOUT` | `5s` | Admission wait limit; full queues reject with HTTP 429. Accepted streaming waiters get lifecycle/heartbeat frames; later timeout uses an SSE error |
| `PRISM_REQUEST_TIMEOUT` | `3m` | End-to-end channel deadline, including queue and warmup |
| `PRISM_HTTP_TIMEOUT` | `20s` | Individual HTTP call limit, including material-provider calls |
| `PRISM_POLL_MIN` | `100ms` | Initial poll delay |
| `PRISM_POLL_MAX` | `1s` | Exponential poll backoff ceiling; must be ≥ minimum |
| `PRISM_STREAM_KEEPALIVE` | `10s` | SSE heartbeat interval after admission |
| `PRISM_SANDBOX_TTL` | `2m` | Maximum context lifetime, also bounded by resource/material expiry |
| `PRISM_KEEPWARM_INTERVAL` | `1m` | Sweep used idle slots; probe valid contexts, rebuild expired ones |
| `PRISM_CACHE_TTL` | `10m` | Conversation cache lifetime |
| `PRISM_CACHE_BYTES` | `67108864` | Logical retained cache byte limit; one entry/chain ≤¼ of this limit |
| `PRISM_CACHE_ENTRIES` | `2000` | LRU entry count limit |
| `PRISM_IMAGE_INLINE` | `false` | After validated upload, supply a shell restoration/view_image instruction for headless workspaces. Newly materialized base64 capped at 48 KiB/image and 96 KiB per preprocess call; cached inline history also counts toward the chain byte limit; larger inputs return 413 |
| `PRISM_IMAGE_MAX_BYTES` | `10485760` | Maximum decoded/downloaded image bytes |
| `PRISM_ALLOW_REMOTE_IMAGES` | `false` | Enable HTTPS downloads of public image URLs |
| `PRISM_MATERIAL_URL` | unset | Trusted provider POST endpoint for browser contexts/fresh headers |
| `PRISM_MATERIAL_BEARER` | unset | Optional authorization for that provider; kept out of JSON config |
| `PRISM_MATERIAL_HEADERS_ONLY` | `false` | When true, material URL only supplies per-call headers (Cookie / User-Agent / openai-sentinel-token); local project/sandbox warmup still runs in-process |

An accounts file contains objects with `id` (optional stable provider identity),
`user_id`, `session_token`, and `access_token`. `user_id` is mandatory when building
contexts locally (no provider or headers-only provider). Full material mode
requires captured metadata `userId`; a configured `user_id` must match it. With a material provider, tokens may be omitted
if the provider returns the real Cookie header for every upstream call. Default
account IDs are `account-0`, `account-1`, etc. Store the file outside Git with mode
0600. No credentials example is populated here. Never commit `.env`, `.api_key`,
`.admin_secret`, browser storage state, or captured material. Rotate static cookies
through the supervisor and restart only the isolated process; material providers
can handle session refresh themselves.

## Live material provider contract

The later `chatgpt-prism2api` reference describes strict one-use request-verification
tokens and browser-bound metadata. Merely supplying cookies does not demonstrate
compatibility with that protocol. For that deployment, set `PRISM_MATERIAL_URL` to
an operator-owned browser/provider service implementing this contract. This is an
integration hook, not a bundled login/verification service and not a direct drop-in
client for the reference sidecar's `/login` action API.

The configured provider endpoint must be HTTPS or loopback HTTP, without URL
credentials or query/fragment. Requests have `Content-Type: application/json` and,
when configured, `Authorization: Bearer <operator-provided value>`.

For `operation:"prepare"`, the body includes `account_id`, `slot_id`, `owner_hash`,
and `path:""`. The provider must return HTTP 200 and a JSON object containing:

- `metadata`: the captured start metadata, including `projectId`, `userId`, `sandbox_url`,
  and `sandbox_token`. Missing identity or mismatched credential/snapshot identity
  fails closed. Only request model/effort override captured metadata. Extra fields such as `codex_listen_snapshot` are preserved.
- `conversation_id`: the captured conversation identity consistent with metadata.
- `expires_at`: Unix seconds; context must remain valid for more than five seconds.

The provider must have completed project/resource/Y-Sweet synchronization and must
isolate contexts by `(account_id, owner_hash, slot_id)`. It must never hand one
owner another owner's project/history. Requests on a slot are sequential, and the
gateway may reuse the context until expiry. A change of owner discards it. Provider
context reclamation is the operator's responsibility.

Before **every** upstream call, the gateway sends `operation:"headers"`, `account_id`,
`path` (including query), and the same owner/slot identifiers. Return HTTP 200 with
`headers`, a string-to-string map. Allowed names are `Cookie`, `User-Agent`,
`openai-sentinel-token`, and `Accept-Language`. Fresh verification tokens are required
for `/api/*`; cookies are required if static account tokens were omitted. The
provider must preserve the browser identity associated with the prepared context
and issue a fresh ticket for each actual call, including poll retries and stop.
No tickets are cached or reused by this gateway. Failure is explicit and bodies
are redacted. Redirects are never followed. Provider calls receive account IDs,
owner hashes, and slot IDs; static cookie values and conversation text are not sent.

Supply real operator-controlled material and validate the current live wire shape
before deployment.

Live cookie bootstrap (operator-owned): a Codex/CPA `access_token` can be exchanged by
calling `GET https://prism.openai.com/auth/session` with Cookie
`prism_oai_access_token=<access_token>` only; a successful response sets
`prism_session_token`. `/api/*` still requires one-use `openai-sentinel-token` tickets
(from an operator material/sentinel sidecar). Do not commit cookies or tokens. `/health/prism` reports `live_verified:true` only after a successful turn against
`https://prism.openai.com`; it stays false for mocks and before the first live success.
Historical live evidence is partial and does not validate the current fixes. If live start fails, compare captured metadata
and provider headers against the current browser protocol without logging tokens.

## Request behavior

Use the existing downstream API key authentication and an auto-channel key.
Keys pinned to Codex/Claude/Grok/Antigravity cannot dispatch to Prism. Keys with
AllowedGroupIDs, PlanAllow, NoAffinityGroupIDs, or any ScopeLimits return HTTP 403
`prism_account_policy_unsupported`, including force mode. Prism credentials have no
stock account mapping, so group/plan/scope budgets and concurrency cannot be authorized. Configure
model limits/pricing for Prism aliases as needed; there is no Prism management UI
or auto-discovered model list in this patch. `PRISM_FORCE` still respects key limits.

Chat messages and Responses input support text, PNG/JPEG/GIF/WebP image blocks,
standard function declarations, function calls, and function results. Images are
validated with decoder dimensions (maximum 50 million pixels), uploaded as raw
image bytes with the correct MIME/edit-access headers, and rewritten to `input_file`
with a `/prism-uploads/` path. Materialized references are cached and preserved
in historical prompts and current attachment blocks, avoiding historical reuploads.
Upload-only vision is experimental: upload success does not prove that the headless
file tree or model can read the file. Historical successful vision smoke used
inline restoration; default non-inline visibility is unverified. Invalid input/upload fails explicitly.
Remote images are opt-in HTTPS only, use no Prism credentials, check all resolved
addresses, dial the validated address, and recheck redirects; private/link-local,
shared/reserved address ranges and non-HTTPS URLs are blocked.

Tools use the prompt-envelope protocol; the gateway returns calls for the client
to execute. It parses `<tool_call>` and JSON `codex-exec`/`tool_call` fences, validates
declared names and JSON object arguments, and honors tool choice and parallel-call
selection. `strict:true` returns HTTP 400 `unsupported_strict_tool`. Parameters must
declare an object schema with valid properties/required/additionalProperties shapes.
It does not execute client tools or validate full JSON Schema argument semantics.
Native function output is normalized too. Tool responses retain call IDs in cached
history. Partial envelopes are buffered until completion. JavaScript custom tools,
Codex `additional_tools`, native web/computer tools, and arbitrary file blocks fail
as unsupported input/tool types. Sampling/output-format/token-limit options are
explicitly rejected rather than silently ignored.

Every SSE write and flush (including heartbeats and terminal frames) has a fresh
10-second write deadline, bounded by the request deadline. Cancellation expires
the active write immediately; write/flush errors cancel upstream work. Writers
must expose net/http write deadlines (middleware wrappers need Unwrap); unsupported
transports are rejected before Prism/key slot acquisition. Shared sandbox prepare
followers can cancel while the leader prepares without canceling that leader.

SSE lifecycle/role events flush after immediate slot or bounded queue admission. Text uses prefix deltas of
upstream snapshots; no true upstream token stream exists. Heartbeat comments run
while HTTP calls block. Tool calls emit Chat deltas or Responses function events.
Chat ends with a finish reason and `[DONE]`, plus usage when `include_usage` is set;
Responses ends with `response.completed`. Midstream failures emit error/failed
events. Client disconnect/deadline triggers a bounded detached stop with the latest
known `turn_state`; stops without a known upstream handle cannot be issued. Start
is never blindly replayed; transient status failures get at most two retries.

`previous_response_id` uses an owner-scoped, byte-bounded cache of items and native
continuity: original slot/account, project, sandbox generation, conversation ID, upstream response ID,
and snapshot session/turn/cursor fields. Start/status snapshots are preferred; a
bounded runtime-debug lookup is a fallback. Follow-ups send `previousResponseId`
and the saved `codex_listen_snapshot`. TTL rotation or invalidation changes the
generation; old handles return HTTP 400 `previous_response_context_expired` before
upstream start. Cache TTL does not extend sandbox lifetime; restart with full
history after expiration. Instructions and quoted text history are
combined into one system item because Prism can discard earlier messages; current
user text blocks are joined. Native restoration alone did not retain textual recall
in the live probe. Project replacement makes old continuations return 400. Miss/expiry/other-owner lookup returns 400; resubmit complete history
when rebuilding a conversation. `store:false` suppresses writes. Restart or another
replica loses cache state; use sticky routing for continuation. Anonymous mode, if
explicitly enabled in stock config, shares the stock anonymous ownership namespace.

## Health, metrics, and troubleshooting

`GET /health/prism` is a redacted local status hook: disabled, unavailable, or
configured with capacity/inflight/queue/warm-slot counts and `paced_waiters`.
In-flight slots include accepted turns waiting for their start budget. `live_verified` becomes true after a successful turn against the real Prism origin;
`last_live_success_at` is the Unix timestamp. Both reset at process restart; a past
success is not a current availability probe. Stock `/health` is unchanged. `GET /metrics/prism` uses standard API-key auth and
Prometheus text output without owner/account labels or credentials.

Counters include `prism_requests`, `completed`, `errors`, `rejected`, `polls`, `stops`,
`stop_errors`, `uploads`, `warmups`, `warmup_errors`, `prompt_tokens`, and
`completion_tokens` (each prefixed `prism_`). `prism_rpm` and `prism_tpm` are rolling
60-second gauges; inflight/queued/warm_slots and logical cache bytes/entries are
gauges. `prism_ttft_seconds_{count,sum}` measures visible streaming output, excluding
initial role/lifecycle events and heartbeats.

Cache counters are `prism_cache_hits`, `misses`, `writes`, `read_tokens`,
`write_tokens`, `read_bytes`, `write_bytes`, `evictions`, and `skipped` (each prefixed
`prism_cache_`). Token counts estimate serialized normalized JSON as bytes/4,
rounded up; repeated reads/writes each count. They describe local context traffic,
not provider KV-cache savings or billing. Upstream usage is preferred; missing usage
sets `prism_estimated:true` and increments `prism_estimated_usage`. Responses/Chat
usage exposes `prism_cache_read_tokens`, `prism_cache_write_tokens`, and
`prism_cache_estimated:true`. Standard `cached_tokens` is taken only from confirmed
provider `usage.input_tokens_details.cached_tokens`, otherwise 0. Invalid negative
counts or cached counts above input tokens fail explicitly. DB cached-token usage
records the same provider-only count. Local replay is not a provider cache hit.

429 admission failures carry `Retry-After: 1`. Upstream 429 cools every slot of that
account for 30 seconds by default (bounded upstream Retry-After is honored); 401/403
cool down for five minutes. Inspect `errors`, `rejected`, and `warmup_errors` before
raising concurrency. A 400 previous-response miss requires full history. A material
error requires provider/session recovery. `generation_failed` also covers upstream
HTTP-200 business errors. Embedded `httpStatus` is surfaced as a redacted
`generation_failed_http_<status>` code; embedded 401/403/429 triggers the same
account cooldown as a transport-level denial. `non_monotonic_output` indicates a rewritten snapshot
after text was already delivered. No upstream error body or Set-Cookie is relayed. Empty/unrecognized completions
fail with `empty_or_unsupported_output`; refusal output is explicitly unsupported
(`unsupported_refusal_output`) and is never returned as an empty success. Supported
message text variants include output_text/text/input_text and top-level message text.
Reasoning-only completions fail explicitly; reasoning/progress output is not exposed.

Disable the experimental channel with `PRISM_ENABLED=false` and `PRISM_FORCE=false`
in its supervisor, then restart that isolated instance. The stock :8080 process
need not be touched for build/test/rollback.


## Experimental live performance (2026-10-02 UTC+8)

Current :8081 uses the headers-only :8091 provider, 10 logical slots, 2 shared
physical contexts, startup prewarm=10, waiters=64, queue=3m, HTTP=60s,
request=5m, poll=100ms–400ms, sandbox TTL=10m, keepwarm=60s, heartbeat=5s,
image inline enabled, and account RPM pacing disabled. Defaults leave sharing,
startup allocation, and pacing disabled. Pacing did not resolve this account's
generic denials and increased latency; no precise safe RPM is established.

Run `python3 tools/prism/live_perf.py e2e` or
`python3 tools/prism/live_perf.py bench --label warm`. The probe reads
`.secrets/api_key_8081` privately and appends redacted summaries to
`logs/prism-live-e2e-evidence-v2.txt` / `logs/prism-perf-10conc.txt`. Meaningful TTFP
excludes role/events/heartbeats; a successful Chat SSE needs a terminal event and
DONE. TPS is completed characters / four / batch wall time. Suite order and the
upstream gate can affect results: isolated probes after a quiet window are needed
to separate protocol failures from authorization denials.

Final-binary three-turn recall passed with native snapshots retained. A readable
480×240 PNG passed OCR and shape recognition after correcting multipart upload to
raw bytes and enabling bounded inline restoration, with low reasoning effort.
Upload returned storage references but no recognized native image URL; headless
viewing uses the restored workspace file rather than a browser-owned Y-Sweet tree.

Final ten-way run: 4/10 completed; six embedded HTTP 403 denials, despite a normal
service-issued session refresh using existing access material. Completed-stream
TTFP p50/p95=9.93/12.57s; rough aggregate TPS=58.35; all ten client streams opened.
Single-turn PONG reached meaningful content in 6.92s versus prior 20.01s.
The final health flag is true from successful turns; it is not an availability probe.

Controls at lower concurrency, fully sequential turns, and paced starts also
encountered 403. JWTs were unexpired and sampled tickets distinct, but the upstream's
generic denial cannot distinguish verification/account policy or establish a precise
limit. Recover accepted operator/browser material or upstream authorization before
repeating sustained capacity acceptance. Keep :8080 running during all restarts.
