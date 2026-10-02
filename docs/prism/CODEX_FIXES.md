# Prism review fixes

Date: 2026-10-02, Asia/Taipei. This supplements the historical independent
[CODEX_REVIEW.md](CODEX_REVIEW.md). Work started from the existing staged and
unstaged Prism implementation; no deployment, production restart, or live account
request was performed. All new tests use synthetic credentials and local mocks.

## Findings and current status

| Finding | Status | Change and regression evidence |
|---|---|---|
| H1 | Fixed by fail-closed authorization | Prism rejects any key with AllowedGroupIDs, PlanAllow, NoAffinityGroupIDs, or ScopeLimits before upstream admission, including force mode and both facade endpoints. Restricted keys return 403 `prism_account_policy_unsupported`. Context-level policy tests and actual auth middleware with SQLite-backed keys verify zero upstream starts. Unrestricted static-key routes remain covered by the facade smoke. |
| H2 | Fixed | Every SSE frame, heartbeat, terminal marker, write and flush uses a fresh 10-second deadline capped by the request deadline. Cancellation expires the active socket operation; write/flush errors cancel upstream work. Flush errors propagate through Gin's wrapped writer. Cancellation callbacks are joined before resetting deadlines; heartbeat shutdown can finish. Unsupported deadline transports are rejected before key/Prism slot acquisition. Tests block initial writes/flushes, later text writes and heartbeat flushes, then verify handler exit, upstream stop, zero active/inflight, and released key permits. Separate tests cover write/request budgets and unsupported writers; existing real HTTP disconnect/flush tests also pass. |
| H3 | Fixed | Full material preserves captured identity/context, including snapshot, sandbox URL/token and frontend origin; only model/effort are overlaid for a fresh turn. Missing userId, configured-user mismatch, or inconsistent snapshot fields fail closed. Locally built contexts require explicit credential user_id / PRISM_USER_ID. Tests cover full material with empty credential UserID, preservation of context, mismatches, and separate token-only and headers-only modes. |
| M1 | Fixed | Standard cached_tokens comes only from provider input_tokens_details.cached_tokens, otherwise 0. Local history byte estimates remain prism_cache_read_tokens / prism_cache_write_tokens with prism_cache_estimated:true. prism_estimated describes request-total fallback separately. Provider cached counts below 0 or above input tokens fail explicitly. DB usage records the provider-only cached count. Tests cover facade separation even when local estimates exceed provider input, provider confirmation/missing fields and invalid counts. |
| M2 | Fixed | Private continuity records bind to a unique successful sandbox preparation generation, copied from shared contexts. TTL rotation/invalidation expires old handles with 400 `previous_response_context_expired` before upstream start. No snapshot migration is claimed. Regression tests cover TTL rotation and exclusive/shared invalidation; native-continuity regressions still pass. |
| M3 | Partially fixed | Cached history now contains materialized attachment references/restoration instructions rather than original image URLs. Follow-ups preserve historical file markers and native attachment blocks and do not reupload the old image. The default non-inline regression verifies the reference and exactly one upload across two turns. Headless file visibility and raw-history inline budget prioritization remain open; see below. |
| M4 | Fixed | Shared preparation serializes through a context-selectable semaphore. Its state mutex is used only briefly to publish/invalidate generations, never across remote I/O. Invalidation no longer waits for the preparation leader. Tests cover canceled semaphore wait and a stalled remote leader whose follower cancels, releases its slot, and leaves the leader/shared context usable. Existing shared owner/concurrency tests and race checks pass. |
| M5 | Fixed through explicit rejection | Chat and Responses strict:true declarations return 400 `unsupported_strict_tool`. Declaration parameters must have object-schema shapes; malformed properties/required/additionalProperties fail. Non-strict tools validate declared names and JSON object arguments, without claiming full JSON Schema argument validation. |
| M6 | Fixed through explicit output contract | Message output_text/text/input_text variants, omitted assistant role, and top-level message text are extracted. Empty/unknown/reasoning-only terminal output fails explicitly; refusal output returns `unsupported_refusal_output`. Unsupported output never increments completed or writes cache. Refusal/reasoning facade rendering is not implemented or claimed. |
| M7 | Fixed for current reporting | TEST_REPORT labels local mock verification and historical live smoke/partial evidence separately; removes strict-schema, provider-cache and performance overclaims. It distinguishes first frame from meaningful output, and TPM counters from output TPS. Historical evidence remains historical and is not attributed to this source revision. |
| L3 | Documentation corrected | RUNBOOK live_verified/cache descriptions and DESIGN status agree with current semantics. |

## Remaining gaps and compatibility changes

- M3: Preserving an upload reference does not prove the model can access a
  headless workspace file. Upload-only vision remains experimental and unverified;
  historical successful vision evidence used inline restoration. No Y-Sweet file
  tree registration was added. Raw historical images in a newly supplied full
  message array still consume the inline budget in history order; prioritizing
  current images remains open. Cached inline history is retained under the cache
  chain byte limit; the 96 KiB materialization limit applies to newly processed
  images, not a cumulative budget across cached turns.
- H1 intentionally restricts compatibility: group/plan/scope-limited keys cannot
  use Prism until credentials have a verified stock account mapping and matching
  scope accounting/concurrency. This is an explicit denial, not a silent bypass.
- H3 requires user identity configuration for token-only/headers-only setups that
  previously sent an empty userId. Full material must supply captured userId.
- M2 intentionally expires native continuations after sandbox rotation, even if
  the facade cache entry is still alive. Clients must restart with full history.
- M5/M6: strict tools, refusal facade output and reasoning/progress output remain
  unsupported. They are rejected where required rather than silently degraded.
- L1 remains open: Close does not separately drain active Run/stop cleanup.
  L2 remains open: warm_slots counts state flags rather than expiry-aware available
  slots, and keepwarm does not guarantee continuous readiness.
- No live tools/vision/capacity matrix, sustained slow-reader load test, production
  health check, deployment or full-repository test suite was run for these fixes.
  Race PASS covers these mock suites; it is not proof of upstream protocol or
  production capacity. Historical 10-way live success remains 4/10.

## Verification

The adapted diagnostic assertions were first run expecting correct behavior:
H1/H2/H3 failed before their fixes. M1/M2/M3/M5/M6 also failed before their fixes;
the original mutex-wait M4 regression failed before replacing that wait. Temporary
failure logs: `/tmp/prism-fixes-before-p0.log`, `prism-fixes-before-p1.log`, and
`prism-fixes-before-m4.log`. These are synthetic diagnostics, not live evidence.

Required suites ran successfully. Final recorded runs add `-json` to capture exact
pass counts, with the same packages, selection and uncached execution:

```bash
GIN_MODE=release go test ./internal/prismchannel/... ./proxy/ -run 'Prism|prism|Feature' -count=1
GIN_MODE=release go test -race ./internal/prismchannel/... ./proxy/ -run 'Prism|prism|Feature' -count=1

# Final runs after all regression additions:
GIN_MODE=release go test ./internal/prismchannel/... ./proxy/ -run 'Prism|prism|Feature' -count=1 -json
GIN_MODE=release go test -race ./internal/prismchannel/... ./proxy/ -run 'Prism|prism|Feature' -count=1 -json

git diff --check
git ls-files -- .secrets
```

| Final run | prismchannel | proxy | Total |
|---|---|---|---|
| Ordinary | 39 top-level + 31 subtests PASS | 12 top-level + 12 subtests PASS | 51 top-level, 94 including subtests; 2 packages PASS |
| Race | 39 top-level + 31 subtests PASS | 12 top-level + 12 subtests PASS | 51 top-level, 94 including subtests; 2 packages PASS; no detected races |

The proxy selection includes two existing non-Prism Feature tests. No tests in
these selected runs failed or skipped. Loopback listeners worked without an
approval/escalation retry. A temporary compile error in an added test used a
nonexistent key-inflight helper; it was corrected to inspect the existing limiter
before the final successful runs. Final JSON records are
`/tmp/prism-fixes-test.jsonl` and `/tmp/prism-fixes-race.jsonl`.
`git diff --check` passed. `git ls-files -- .secrets` returned no tracked files.

## Files changed for these fixes

- Channel: client.go, config.go, sandbox.go, session.go, tokencache.go,
  translate.go, types.go under internal/prismchannel/.
- Channel tests: new review_test.go; identity fixtures adjusted in client_test.go,
  session_test.go and translate_test.go.
- Facade: proxy/prism_handler.go, prism_stream.go, prism_test.go, and new
  prism_review_test.go.
- Documentation: CODEX_FIXES.md, CODEX_REVIEW.md status note, TEST_REPORT.md,
  RUNBOOK.md, DESIGN.md, and the unpopulated PRISM_USER_ID option in .env.example.

No real credential files or tokens were read, printed, added or staged. Existing
workspace changes were preserved. The reference tree `/workspace/codex-prism`
was only read; `.secrets/` was untouched and remains untracked. No commit was made.
