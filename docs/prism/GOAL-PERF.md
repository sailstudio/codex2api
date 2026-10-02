# Goal (Codex goal mode) — Prism live gaps + performance

> Date: 2026-10-01 (Asia/Shanghai)
> Tree: `/workspace/codex2api-run`
> Reference (read-only): `/workspace/codex-prism`
> Prior live evidence: `logs/prism-live-e2e-evidence.txt` (TTFP ~20s cold; previous_response_id FAIL 502; vision "Unavailable"; live_verified=false)

## Outcome

Close remaining **live** Prism gaps and hit a **capacity/latency** target on experimental **:8081**, self-iterating until verified or hard upstream limits are documented.

When done, evidence must show: code patches + unit/mock tests green; live re-test evidence; 10-concurrent bench with p50/p95 TTFP; docs updated; secrets untracked; stock **:8080** still healthy.

## Goals

### 1. Fix remaining live gaps (from last e2e)

1. **Multi-turn / session continuity**
   - `previous_response_id` must continue beyond local item-cache replay.
   - Persist and reuse Prism conversation continuity: sticky `conversationId`, `codex_session_id` / turn ids inside `codex_listen_snapshot` (or equivalent upstream fields), account affinity as needed.
   - Prove with live `/v1/responses` store + follow-up using `previous_response_id` (expect 200 continuity, not 502), **or** document a proven upstream hard limit with redacted evidence.

2. **Vision**
   - Improve beyond 1×1 PNG → model text `"Unavailable"` where possible.
   - Use a better fixture (small real JPEG/PNG with readable content, not a 1×1).
   - Keep SSRF/upload path; record whether failure is fixture, upload mapping, or upstream vision capability.

3. **`/health/prism` `live_verified`**
   - Prefer flipping `live_verified` to true after a successful live turn (or document why it stays false by design and add an explicit operator-facing signal).

### 2. Performance / capacity target

- Sustain **10 concurrent** Prism chat streams on :8081 without cascade failures (most complete; no mass 429/502 death spiral).
- Optimize **TTFP** (time to first meaningful token/chunk) vs prior ~**20s** baseline when warm path works.
- Optimize **TPS** (tokens/sec, or chars/sec proxy if usage missing).

Techniques to consider (from `/workspace/codex-prism` CAPACITY/TTFB plans + current code):

- Sandbox warm pool size / keepwarm interval (avoid cold warmup on critical path)
- Admission queue + `PRISM_ACCOUNT_CONCURRENCY` / `PRISM_MAX_WAITERS` (current live env uses concurrency=1 — too low for 10-way)
- Parallel poll tuning (`PRISM_POLL_MIN`/`MAX`)
- Keepalive vs early progress SSE (emit progress before full completion)
- HTTP connection reuse
- Per-account semaphore sizing vs single-account live pool
- Headers-only material provider latency (`PRISM_MATERIAL_HEADERS_ONLY`, provider on :8091)

### 3. Self-iterate

Implement → unit/mock test → rebuild/restart :8081 → live re-test → 10-conc bench → fix → re-bench until targets met **or** hard upstream limits clearly proven and documented.

## Environment (reuse; do not invent secrets)

- Secrets already in `/workspace/codex2api-run/.secrets/` (`cpa-account.json`, prism materials, `env.prism8081`, `start-prism8081.sh`, material provider). Keep mode **600**; never commit or echo tokens/JWTs/cookies.
- Stock **:8080** leave healthy; Prism experimental on **:8081** (rebuild/restart as needed via `.secrets/start-prism8081.sh`).
- Material provider already on **:8091** (headers-only). Do not invent new credentials.
- API key for :8081: `.secrets/api_key_8081` (use in Authorization header; never print).

## Verification

1. `go test ./internal/prismchannel/... -count=1`
2. `GIN_MODE=release go test ./proxy/ -run 'Prism|prism' -count=1`
3. Rebuild `.deploy/prism/codex2api-prism`; restart **:8081** only (not :8080)
4. Live e2e → `logs/prism-live-e2e-evidence-v2.txt` (redacted; no secrets)
5. Bench → `logs/prism-perf-10conc.txt` with: completed/error counts for 10 concurrent streams; p50/p95 TTFP; rough TPS/chars-sec; warm vs cold notes
6. `curl -sS http://127.0.0.1:8080/health` still ok
7. Update `docs/prism/DELIVERY.md` and briefly `docs/prism/RUNBOOK.md`
8. `git status` shows no `.env` / `.api_key` / `.admin_secret` / `.secrets/` staged

## Constraints

- Work in `/workspace/codex2api-run`; read-only reference `/workspace/codex-prism`
- Do not invent credentials; do not commit secrets
- Prefer additive Prism-channel patches over rewriting main Codex path
- Redact all evidence logs (no cookies, JWTs, api keys, account emails if sensitive)
- Zero secrets in final report text

## Success criteria

- 10-way concurrent mostly succeed (document any upstream single-account limits)
- TTFP improved vs prior ~20s when warm path works (or document irreducible upstream floor)
- Multi-turn fixed **or** gap clearly proven upstream with evidence
- Unit/mock tests still green; stock :8080 healthy

## Stop when

Targets met, or no defensible local fix remains without new upstream capability / additional accounts — then document remaining gaps clearly in DELIVERY + evidence files.


## Execution result — 2026-10-02 (UTC+8)

Completed through the documented upstream-blocker stop condition. Final-binary
three-turn continuity and readable PNG OCR/shape recognition pass; health records
live verification. Stock :8080 remains healthy, and tests/secret checks pass.
Final warm ten-way run completes 4/10: six embedded HTTP 403 denials persist across
concurrency, pacing, sequential-turn, and legitimate session-refresh controls.
Ten-way success remains unmet; no exact policy/quota ceiling is claimed.
See [DELIVERY.md](DELIVERY.md), [live evidence](../../logs/prism-live-e2e-evidence-v2.txt),
and [performance evidence](../../logs/prism-perf-10conc.txt) for results and limits.
