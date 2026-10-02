# Prism performance delivery — 2026-10-02 (UTC+8)

Multi-turn continuity, readable-image recognition, and live health verification
now pass. Warm standalone PONG TTFP improved from the recorded 20.01s baseline to
6.92s. Ten-way success remains blocked by upstream denials with the supplied account
and verification material: the final warm batch completed **4/10**, with six embedded
HTTP 403 errors. This meets GOAL-PERF's documented upstream-blocker stop condition;
sustained ten-way success is **not achieved**.

The rebuilt binary is `.deploy/prism/codex2api-prism`, running on :8081 through
`.secrets/start-prism8081.sh`. Stock :8080 was neither replaced nor restarted and
remains `status:ok`, available=1, total=1. Reference trees under
`/workspace/codex-prism` were read only. Existing staged work was preserved; this
work did not stage or commit source changes or credentials.

Implementation:

- The bounded owner cache retains items and private native continuity: original
  slot/account affinity, project, sticky conversation ID, upstream response ID,
  and `codex_session_id` / `last_turn_id` / cursor fields. Start/status/payload
  snapshots are preferred; runtime-debug is a bounded fallback. State shares cache
  TTL, LRU, and byte limits. Follow-ups send the native previousResponseId and saved
  snapshot. Replaced projects invalidate old handles; fresh requests get fresh
  conversations except when a browser material provider owns the conversation.
- Prism discarded earlier messages and separate text blocks. Instructions and
  quoted history now form one system item; current user text is joined. Incremental
  native restoration alone did not retain recall, so full textual history is kept
  alongside native session state. Three live Responses turns on the final binary
  returned ACK, MAPLE-4827, MAPLE-4827, with three snapshots and two cache hits.
- Image upload now follows the browser's **raw image bytes** protocol, correct MIME,
  file headers, edit-access flag, and `/prism-uploads/` path. The previous multipart
  transport was incorrect. Storage references are validated and counted. Optional
  bounded inline restoration/view_image instructions retain SSRF validation and
  upload. Base64 limits are 48 KiB/image and 96 KiB/request; oversize returns 413.
- Optional startup prewarm reserves idle slots with two workers. An opt-in physical
  sandbox pool shares contexts across logical slots while keeping owner projects
  separate. Accepted queue waiters receive lifecycle events and heartbeats before
  slot availability. Existing HTTP connection pools are reused. Optional per-account
  start pacing is bounded by admission/request deadlines and disabled in the selected
  deployment: it did not resolve the observed gate and increased latency.
- Successful real-origin turns set live_verified and last_live_success_at; mock
  turns cannot. Both reset on restart. They record a historical success, not current
  availability. The final process reports live_verified=true, ten warm slots.
- Embedded response.payload.httpStatus is classified safely even in HTTP-200 error
  envelopes. Embedded 401/403/429 cool every account slot without blind start retries.
  Authorization/rate denials retain valid warm contexts, avoiding allocation cascades.
  Upstream bodies, cookies, and private snapshots are not exposed in errors or health.

Evidence: `logs/prism-live-e2e-evidence-v2.txt` and `logs/prism-perf-10conc.txt`.
Both retain failed iterations and controls. Probe: `tools/prism/live_perf.py`;
fixture: `testdata/prism/vision-readable.png` (480×240, 1,811-byte palette PNG).

| Live check | Result |
|---|---|
| Final-binary Responses store + two follow-ups | All HTTP 200, correct recall; 6.26s / 5.41s / 5.73s |
| Raw PNG upload + inline viewing, low reasoning effort | HTTP 200; text ORBIT 739, red square, blue circle; 49.74s meaningful TTFP |
| Upload mapping | One storage-reference response; no recognized native URL field. Headless materialization still uses inline restoration |
| Image-first control before raw-upload fix | Pending for 5m, no text; bounded stop succeeded. Historical failure retained |
| Warm standalone PONG | 6.92s meaningful TTFP; first frame 4.3ms |
| Final health | Configured, capacity=10, warm_slots=10, live_verified=true; :8080 healthy |
| Material checks | Active JWTs unexpired, three sampled tickets distinct |
| Normal session refresh using existing access material | Service-issued cookie received, same user scope, private file mode 600; denials persisted |

Final selected configuration: headers-only :8091 provider; 10 logical slots,
2 physical contexts, prewarm=10, max waiters=64, queue=3m, request=5m, HTTP=60s,
poll=100–400ms, sandbox TTL=10m, keepwarm=60s, heartbeat=5s, image inline enabled,
account RPM pacing disabled. Shared pool and prewarm remain disabled by default.

| Ten concurrent client streams | Complete/error | Meaningful TTFP p50/p95, seconds | Rough aggregate TPS |
|---|---|---|---|
| Initial separate sandboxes | 4 / 6 | 9.34 / 12.96 | 57.72 |
| Shared pool=2, all ten slots warm | 4 / 6 | 8.92 / 9.81 | 74.16 |
| Post-cooldown warm ten-slot run | 4 / 6 | 8.94 / 13.58 | 54.18 |
| Final, refreshed session, final binary | **4 / 6** | **9.93 / 12.57** | **58.35** |

Final wall=12.82s; peak in-flight=10; all ten streams opened before the first terminal
response; first-frame p50/p95=4.0/4.8ms. Those role/lifecycle frames are not meaningful
TTFP. Percentiles use **completed** streams, with every error counted separately.
TPS is completed characters / 4 / batch wall seconds, not billing tokens. These
small-sample comparisons are directional; no sustained throughput claim is made.

Additional controls did not solve the blocker: limiting upstream concurrency to
four completed 2/10 after cooldown; serializing ticket issuance completed 4/10 and
increased latency (that experiment was removed); pacing four starts per 63s still
completed 4/10 over 135.95s; fully sequential upstream turns completed four, then
the fifth received 403 and the remaining queue timed out. In that sequential run,
all ten client streams were open, peak slot queue=9, and completion was 4/10 over
180.14s. No exact RPM or concurrency ceiling can be inferred from these generic
errors. Neither overlapping turns alone nor expired cookies alone explains them.

The remaining hard blocker is upstream authorization/verification or account policy
with the available material. Additional accepted browser material/upstream support
or working operator-provided accounts is required for a reliable ten-way acceptance
run. Unexpired JWTs, distinct tickets, and a normal cookie refresh do not prove that
all browser verification requests are accepted. No tokens were fabricated and :8091
was retained. Vision now works through validated raw upload plus inline viewing;
a browser/Y-Sweet file-tree integration remains outside this channel.

Validation passed: required Prism unit and proxy mock suites, race tests for both,
repository-wide GIN_MODE=release go test ./..., and go vet for Prism/proxy/config.
Regression coverage includes native snapshot/response-ID affinity, fresh conversation
separation, ten concurrent mock turns on two prewarmed contexts, owner isolation,
raw-image decoding at the mock upload endpoint, inline restoration, paced admission,
and embedded authorization cooldown without discarding a warm context. Final
whitespace, secret-scan, health, and index checks are recorded in the live evidence.

Credentials and runtime artifacts remain ignored. Continuation is process-local;
use sticky routing across replicas. Shared contexts are opt-in and best suited to
one owner; changing owners can allocate additional isolated projects, requiring
operator cleanup upstream. See RUNBOOK for knobs, probes, and recovery.
