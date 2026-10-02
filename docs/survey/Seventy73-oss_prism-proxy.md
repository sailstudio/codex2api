# prism-proxy

**A local reverse proxy that puts an OpenAI-compatible API in front of
[prism.openai.com](https://prism.openai.com) — OpenAI's AI LaTeX editor, codename
*Crixet*.**

Point any OpenAI client at `http://127.0.0.1:8787/v1` and requests are replayed
as Prism `response_with_tools` turns against a Prism sandbox.

> **Educational project — read this first.**
>
> This is a **learning exercise in reverse-engineering an undocumented web API**,
> written to understand how a modern agentic backend works: how it provisions
> sandboxes, how it streams progress, how it relays tool calls. It is published
> as a study of *protocol design*, not as a product.
>
> * **It is not affiliated with, endorsed by, or supported by OpenAI.** "OpenAI",
>   "Codex" and "Prism" are trademarks of their respective owners.
> * **It violates no licence it claims to honour, but it may violate the
>   upstream terms of service.** Automating an account you do not own, or
>   redistributing access to a paid service, is not what this is for. Run it
>   against **your own account**, for your own study.
> * **There is no warranty.** The upstream is undocumented and changes without
>   notice; this code can break at any time, and does.
> * **Do not deploy this publicly or commercially.** Running it as a shared
>   endpoint for other people is both an abuse of the upstream and a credential
>   leak waiting to happen.
>
> If you are here to learn how an agent backend is put together, the
> [How it works](#how-it-works) and [Notes and limitations](#notes-and-limitations)
> sections document what was measured — including the parts that turned out to
> be impossible. If you are here for free API access, this is not that.

```
OpenAI client  ->  http://127.0.0.1:8787/v1/chat/completions
                       |
                       +- Prism session cookie (anonymous or signed in)
                       +- Prism project  (POST /api/projects)
                       +- Prism sandbox  (POST /api/backend/1/new)
                       +- workspace sync (resources-token -> /api/y -> token)
                       +- turn           (POST /api/llm/response_with_tools_start
                                          + /response_with_tools_status polling)
```

## Requirements

* Node.js 20 or newer (tested on 22). No dependencies - pure `node:` builtins.
* Network access to `prism.openai.com`.
* A Prism account. The proxy will mint an **anonymous** session by itself, which
  is enough to exercise projects, sandboxes, LaTeX rendering and (once the
  sandbox is fully warmed) LLM turns. A **signed-in Plus/Pro** session is what
  you want for real work: it is the account whose quota and model access apply,
  and it avoids the flakier anonymous path. Run `npm run login` for that.

## Quick start

```
cd prism-proxy
npm run login      # capture your signed-in prism.openai.com cookies
npm run doctor     # verify session -> project -> sandbox -> sync -> codex
npm start          # serve on http://127.0.0.1:8787
```

Then, from any OpenAI-compatible client:

```
curl http://127.0.0.1:8787/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hello"}]}'
```

No API key is required by default. To require one, set `PRISM_PROXY_TOKEN` and
send it as `Authorization: Bearer <token>` or `x-api-key`. When a key is set,
every route except `/healthz` and `/v1/models` requires it, including the
`/admin/*` endpoints.

Check the proxy at any time with:

```
curl http://127.0.0.1:8787/healthz?probe=1
```

`probe=1` asks Prism whether it is accepting work, so you can tell an idle but
healthy proxy from one whose upstream is in maintenance.

## Logging in

`npm run login` supports three modes:

| Mode | Command | Notes |
| --- | --- | --- |
| CDP | `node src/login.mjs --cdp 9222` | Reads cookies directly from a Chromium browser started with `--remote-debugging-port=9222` |
| stdin | `node src/login.mjs --from-stdin` | Pipes a Cookie header or a JSON cookie array in |
| paste | `node src/login.mjs` | Prompts you to paste the Cookie header |

To get the value: open prism.openai.com while signed in, DevTools -> Network ->
any `/api/` request -> copy the `Cookie` request header. The important cookies
are `prism_session_token` and `prism-did`.

Cookies are stored in `prism-cookies.json` (git-ignore it).

## Endpoints

### OpenAI-compatible

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/v1/chat/completions` | Chat Completions, streaming and non-streaming; surfaces Prism's own tool calls |
| `POST` | `/v1/responses` | Responses API |
| `POST` | `/v1/messages` | Anthropic Messages API (for Claude Code style clients) |
| `GET` | `/v1/models` | Lists available Prism models |

### Prism-native passthrough

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/v1/prism/render` | Compile LaTeX; returns a PDF |
| `POST` | `/v1/prism/files` | Upload a project file (raw body, `?name=`/header) |
| `POST` | `/v1/prism/thumbnail` | Link a file as the project thumbnail |
| `GET` | `/v1/prism/files` | List the sandbox workspace (`entry-files`) |
| `GET` | `/healthz` | Session, project, sandbox and queue status |
| `GET` | `/healthz?probe=1` | Same, plus a live check that Prism is accepting work |
| `GET` | `/admin/session` | Raw Prism `/auth/session` payload |
| `POST` | `/admin/cookies` | Import cookies at runtime (`{"cookies":[...]}`) |
| `POST` | `/admin/warm` | Provision the project + sandbox up front |

`/v1/prism/render` accepts the sandbox render body
(`{"mainDocument": ...}`) and compiles through the same warm sandbox.

`/v1/prism/files` mirrors the web app's `POST /api/project-files/upload`:
the bytes are the **raw request body** and the metadata travels in headers
(`x-prism-file-name`, `x-prism-file-size`, `x-prism-project-id`,
`x-prism-require-project-edit-access`). Send JSON and Prism answers
`413 Upload body was incomplete`, so `curl --data-binary` is the right tool:

```bash
curl -X POST 'http://127.0.0.1:8787/v1/prism/files?name=main.tex' \
  -H 'content-type: text/plain' \
  -H 'x-prism-file-name: main.tex' \
  --data-binary @main.tex
```

`/v1/prism/thumbnail` posts `{"thumbnail_uuid": ...}` to
`PATCH /api/projects/{uuid}/thumbnail`. Prism only accepts a file that is
already linked to the project and answers
`400 Thumbnail file is not linked to project` otherwise; the proxy surfaces
that message rather than masking it. `GET /v1/prism/files` lists what the
sandbox workspace currently holds.

**Render does not take LaTeX source.** `/v1/prism/render` compiles whatever is
already present in the sandbox workspace, so `{"mainDocument": "main.tex"}` only
works once that file is really there. Prism keeps two separate stores - the
project file store written by `/api/project-files/upload`, and the
Y-Sweet-backed workspace the compiler reads - and in testing an upload did **not**
by itself populate the workspace (`entry-files` stayed `{"files":[]}`, and
`render-status` spun until it 500'd). The proxy therefore checks `entry-files`
first and answers `422 empty_workspace` / `422 missing_document` immediately
instead of hanging until the upstream times out.

## Reasoning effort

`gpt-5.6-sol` advertises six levels in the upstream model catalog:
`low`, `medium`, `high`, `xhigh`, `max`, `ultra`. Measured against the real
upstream (four questions that need multi-step arithmetic, marker-matched to the
codex session file each turn created, reading codex's own
`reasoning_output_tokens`):

| Requested | Recorded by codex | `reasoning_output_tokens` | Honoured |
| --- | --- | --- | --- |
| `low` | `low` | 122 | yes |
| `medium` | `medium` | 140 | yes |
| `high` | `high` | 154 | yes |
| `xhigh` | `xhigh` | 237 | yes |
| `max` | `low` | 126 | **no - downgraded upstream** |
| `ultra` | `low` | 139 | **no - downgraded upstream** |

So `low` through `xhigh` genuinely change how much the model thinks, and
`xhigh` is the strongest level that actually takes effect. `max` and `ultra`
are accepted by the catalogue but silently downgraded to `low` by the upstream,
which is why raising the level past `xhigh` makes answers no better and can make
them worse. The proxy passes the level through unchanged rather than rewriting
it, so a client can see this for itself; `minimal` is folded to `low` and
`extra-high` to `xhigh` as aliases.

Note that codex reports `usage` as `null` at the wire level, so these token
counts come from the sandbox's own session log, not from the API response.

## Configuration

All configuration is environment variables.

| Variable | Default | Meaning |
| --- | --- | --- |
| `PRISM_PROXY_HOST` | `127.0.0.1` | Listen address |
| `PRISM_PROXY_PORT` | `8787` | Listen port |
| `PRISM_PROXY_TOKEN` | *(empty)* | When set, clients must present this bearer token |
| `PRISM_BASE` | `https://prism.openai.com` | Prism origin |
| `PRISM_MODEL` | `gpt-5.6-sol` | Default model when the request omits one |
| `PRISM_REASONING` | `xhigh` | Default reasoning effort; see "Reasoning effort" below |
| `PRISM_COOKIE_FILE` | `prism-cookies.json` | Where the session cookies live |
| `PRISM_STREAM_NARRATION` | `1` | Stream Prism's progress narration as visible content; `0` keeps streamed text equal to the final answer |
| `PRISM_CONCURRENCY` | `1` | Simultaneous Prism turns (keep at 1) |
| `PRISM_ALLOW_ANONYMOUS` | `1` | Set to `0` to refuse anonymous sessions |
| `PRISM_VERBOSE` | `0` | Set to `1` for per-request debug logs |

## Using it with real clients

**OpenAI SDK (JS/Python)** - set `baseURL`/`base_url` to
`http://127.0.0.1:8787/v1` and any API key string.

**Claude Code / Anthropic SDK** - set `ANTHROPIC_BASE_URL` to
`http://127.0.0.1:8787` and `ANTHROPIC_API_KEY` to any non-empty string; the
proxy speaks `/v1/messages`.

**Codex CLI** - in `~/.codex/config.toml`:

```toml
model_provider = "prism"
model = "gpt-5.6-sol"

[model_providers.prism]
name = "Prism"
base_url = "http://127.0.0.1:8787/v1"
env_key = "PRISM_PROXY_TOKEN"
wire_api = "responses"
```

## How it works

1. **Session.** `GET /auth/session` reads the current user. With no usable
   cookie the proxy mints an anonymous session via
   `POST /api/auth/anonymous-session`; with a signed-in cookie it uses your
   Plus/Pro identity.
2. **Project.** `POST /api/projects` with a fresh UUID creates the workspace the
   sandbox is bound to.
3. **Sandbox.** `POST /api/backend/1/new` returns
   `{url, token, sandbox_id, sandbox_session_id}`. Every sandbox call carries
   the `X-Crixet-Sandbox-Token` header.
4. **Workspace sync.** This is the part that is easy to get wrong. In order:
   `POST /api/projects/{uuid}/sandbox/resources-token` ->
   `POST {sandbox}resources-token` ->
   `POST /api/y` (Y-Sweet client token) ->
   `POST {sandbox}token` with the raw token ->
   `GET {sandbox}wait-for-sync?wait_ms=10000` until
   `status == "synced"` and both `hasCurrentYSweetToken` and
   `hasSyncedYSweetProvider` are true.
5. **Turn.** `POST /api/llm/response_with_tools_start` with
   `{input, previousResponseId, metadata, conversationId}`. `metadata` carries
   `projectId`, `userId`, `model`, `reasoning_effort`, `sandbox_url`,
   `sandbox_token` and `frontend_origin`. The call answers either
   `{status:"completed", response}` straight away or `{status:"started",
   request_id, turn_state}`, in which case the proxy polls
   `POST /api/llm/response_with_tools_status` with
   `{request_id, turn_state}` until the turn settles.
6. **Recovery.** A freshly provisioned sandbox often answers the first turn with
   `{"reason":"sandbox_reconnecting"}`, and a cold Codex backend can 500 on
   `codex/healthz`. The proxy retries the start call, waits for
   `codex/healthz` to report `{"status":"ok"}`, and reprovisions the sandbox
   when Prism says it is reconnecting.
7. **Client tools.** There is no tools field in the start request, so a client's
   tool list is described to the model in the prompt instead and the reply is
   parsed back into `function_call` items - see "Tool calling" below.

## Notes and limitations

* Prism runs one Codex turn per sandbox, so requests are serialized. Long
  prompts with `xhigh` effort take minutes; the default request timeout is
  5 minutes for the start call and 15 minutes for the whole turn.
* **Continuity: Prism ignores conversation history, so the proxy replays it.**
  Verified live against the real endpoint: `turn_state.prompt` is built from the
  *last* input item only (`"User request:\n<last message>"`), and every turn gets
  a brand new `codex_session_id`. Sending the transcript as earlier array items,
  or passing `previousResponseId`/`conversationId`, does **not** carry context -
  a follow-up question about a number given one turn earlier answers "I don't
  know". `POST /api/codex/conversation-history` also comes back
  `{"items":[],"backendConversationFound":false}`.
  The proxy therefore collapses the request's messages into a single user
  message (`flattenInput`) before sending it, which is the only shape that
  survives. Two turns that only flatten their own messages still cannot see each
  other, so `/v1/responses` additionally remembers each response's transcript and
  replays it when the client passes `previous_response_id`. Both paths are
  covered live by `npm run test:continuity`.
* A client that only sends the newest message with a `previous_response_id` the
  proxy has never seen (for example after a restart) will lose that context; the
  proxy logs `unknown previous_response_id` and answers without it rather than
  failing.
* `usage` is always `null` upstream, so token counts are reported as zero
  unless Prism starts returning them.
* **Streaming: the final answer cannot be streamed, but progress can.** Prism
  has no token stream. The sandbox exposes no streaming route either (probed
  `codex/stream`, `codex/events`, `v1/responses`, `events` and others - all
  404; only `codex/healthz` exists), and `response_with_tools_status` returns
  the assistant text only once the turn completes. A long answer therefore
  arrives in one chunk at the end - measured: 267 Chinese characters as 1 delta
  after 10.2s, and 1532 characters as 1 delta after 26.9s. That part is a hard
  upstream limit; streaming it would mean inventing text.
  What *is* progressive is `codex_live_progress`, and the proxy now uses two
  real streams from it:
  - `eventPreviews[].payload_type == 'agent_reasoning'` -> `reasoning_content`
  - `eventPreviews[].payload_type == 'agent_message'` -> visible content

  `agent_message` is Prism's progress narration ("I'll check the working
  directory first..."). It arrives while the turn is running (measured at 2.3s
  and 13.7s of a ~20s turn) and it does **not** duplicate the final answer
  (verified: 0 overlapping messages). It only appears on turns where the agent
  does work - a pure question with no tool use emitted no narration at all
  (measured: `events=10`, none of them text), in which case the answer is still
  a single chunk. Set `PRISM_STREAM_NARRATION=0` to keep streamed content equal
  to the final answer.
  `reasoningSummaries` is used as a fallback but was frequently empty for whole
  turns, which is why `eventPreviews` is now the primary source. Both windows
  **slide** rather than append (consecutive polls reported 1, 0, 0, 1, 1, 1, 0,
  0, 1 entries), so dedup is keyed on `line_index`, never on list length.
  Streaming and non-streaming responses carry the same narration, so a client
  does not get a different answer depending on whether it asked for a stream.
* A turn that Prism reports as failed (for example `sandbox_reconnecting` after
  the retries are exhausted) is returned as HTTP 502 with the reason, not as a
  200 carrying an empty assistant message.
* **Tool calling is relayed through the prompt.** Prism's start request is
  exactly `{input, previousResponseId, metadata, conversationId}` - there is
  no field for a custom tool list. Rather than fail, the proxy appends a
  directive describing the client's tools and asking for
  `{"tool_calls":[{"name":...,"arguments":{...}}]}`, then parses a reply that is
  *exactly* that envelope back into `function_call` items. Normal
  function-calling semantics are preserved: the model requests the call, the
  **client** executes it, and the client sends the result back as a `tool`
  message / `tool_result` block. The proxy never executes a function and never
  invents a result. Verified live for chat completions, `/v1/responses` and
  `/v1/messages`, including the full round trip (18°C and 27°C returned from
  client-supplied results) and the negative case (a question needing no tool
  returns plain text with `finish_reason: "stop"`).
  Parsing is deliberately strict: a fenced block is accepted, but anything
  without an exact envelope, with an unknown tool name, or with non-object
  arguments is treated as ordinary prose, so a normal answer is never mangled.
  In a stream the envelope is held back rather than leaked as assistant text,
  and the call is emitted as `tool_calls` deltas.
  This is a prompt-level shim, so it is less reliable than a real tools field -
  a model may phrase a call slightly differently and the proxy will pass that
  through as text. Prism's own Codex toolset (`exec`, `apply_patch`,
  `open_file`, web search) runs inside the turn regardless and its calls are
  **not** forwarded: verified live that a turn using `exec` to list files
  still returns a single `message` item, so those internal calls stay invisible
  to the client (only the resulting text is returned).
* **Codex CLI works.** Codex sends its own toolset and drives the loop itself, so
  it needed three extra pieces, all verified live:
  - `instructions` - Codex puts its entire system prompt in this top-level
    Responses field rather than in `input`. It is folded in as a system item so
    the agent keeps its persona and rules (verified with a canary token that only
    appears if the instruction reaches the model).
  - `response.output_item.done` - Codex collects tool calls from these events,
    **not** from `response.completed`. The proxy now announces each output item
    (`output_item.added`, `function_call_arguments.delta/done`,
    `output_text.done`, then `output_item.done`). Without this Codex saw an
    empty turn and never ran the tool.
  - `type:"custom"` tools - Codex sends `apply_patch` as a freeform grammar
    tool, not a JSON-schema function. These were silently dropped; they are now
    described with a single `input` string argument and returned as
    `custom_tool_call`/`{input}` items, with
    `custom_tool_call_input.delta/done` events. A full loop
    (`apply_patch` -> `shell` -> final answer, every call executed by the
    client) completes correctly.
  In a stream the tool envelope is held back rather than leaked as assistant
  text, on the Responses path too.
  Codex fields that have no Prism equivalent (`store`, `tool_choice`,
  `parallel_tool_calls`, `include`, `prompt_cache_key`, `reasoning`) are
  accepted and ignored rather than rejected.
* **Warm-up matters.** The first turn after a sandbox is provisioned is the
  flaky one. Prism answers `{"reason":"sandbox_reconnecting"}` while the
  workspace is still syncing, and a cold Codex backend 500s on
  `codex/healthz`. The proxy absorbs both (retry the start call, wait for
  `codex/healthz`, reprovision on reconnect), which is why an anonymous session
  can in fact complete turns - but a signed-in session is still the better path.
  If turns keep failing, check `/healthz` and run `npm run login`.
* **Maintenance mode.** Prism periodically takes projects and AI features
  offline (`maintenance_mode_type: "full"`). It answers 503 with
  `{"maintenance_mode":true}` on some routes and only a human-readable
  `{"detail": "We're investigating reports that users are not able to compile
  and use AI features"}` on others; the proxy recognises both, returns a 503
  with an explanatory message instead of retrying pointlessly, and reports
  `"ok": false` plus the `maintenance` reason on `/healthz`.
  `/healthz?probe=1` additionally asks Prism directly, so you can tell an idle
  but healthy proxy from one whose upstream is down.
* This is a personal-use bridge over your own Prism subscription. Respect
  OpenAI's terms of service.

## Layout

```
prism-proxy/
  package.json
  src/
    server.mjs        HTTP server, routing, OpenAI/Anthropic translation
    session.mjs       Prism project + sandbox lifecycle and turn driving
    prism-client.mjs  Prism HTTP client (cookies, retries, all upstream calls)
    translate.mjs     OpenAI <-> Prism wire-format conversion
    login.mjs         cookie capture helper
    doctor.mjs        end-to-end connectivity check
  mock-prism-server.mjs  stand-in prism.openai.com for tests
  test-offline.mjs       translation-layer unit tests (no network)
  test-cookies.mjs       cookie import filtering tests
  test-mock-prism.test.mjs  drives the proxy against the mock server
  test-auth.test.mjs     API-key gating tests
  test-proxy.mjs         live request tests against a running proxy
  test-continuity.mjs    live two-turn memory check
  test-upload.mjs        live project-file upload + thumbnail check
  test-render.mjs        live PDF render check
  test-tools.mjs         live tool round trip across all three APIs
  test-tools-edge.mjs    live tool edge cases (unknown names, bad JSON)
  test-tools-stream.mjs  live: envelope must not leak into streamed text
  sim-codex*.mjs         live Codex CLI simulations (SSE, tool loop, chaining)
  watch-and-test.mjs     waits out Prism maintenance, then runs the live suite
```

## Tests

```
npm test                # all offline tests: 183 assertions, no network, no account
npm run test:offline    # translation, flattening, tool relay (60 assertions)
npm run test:cookies    # cookie import filtering (8 assertions)
npm run test:mock       # boots a fake prism.openai.com and drives the whole proxy (106)
npm run test:auth       # API-key gating on protected routes (9 assertions)
npm run test:live       # exercises a real running proxy (needs Prism to be up)
npm run test:continuity # live two-turn memory check
npm run test:upload     # live file upload + thumbnail check
npm run test:render     # live PDF render check
npm run test:tools      # live tool round trip across all three APIs
npm run test:tools-stream # live: tool envelope must not leak into streamed text
npm run test:codex      # live: full Codex CLI simulation (tools, instructions, chaining)
```

`test-mock-prism.test.mjs` starts `mock-prism-server.mjs`, starts the proxy
pointed at it, and asserts the full HTTP contract: routing, auth, chat
completions (streaming and not), the exact upstream request shape
(`input`, `previousResponseId`, `conversationId` and every `metadata`
field), the Responses API, the Anthropic Messages API, file upload (including
that the bytes are the raw body rather than JSON), thumbnail, render
compilation, client-tool relaying (directive injection and envelope parsing),
and the error paths (failed turn, streaming failure frames, empty workspace). This runs with no network access and no Prism account, which is why
it is the primary regression test - the real service is frequently in
maintenance.

