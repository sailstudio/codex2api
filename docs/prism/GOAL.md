# Goal (Codex goal mode)

## Outcome
Ship a production-grade Prism reverse-proxy **channel** as patches on
`/workspace/codex2api-run`, synthesizing best practices from
`/workspace/codex-prism` (especially `oai-prism` + `chatgpt-prism2api`).

When done, evidence must show: design note present, code compiled, unit/mock
smoke tests green for tools + streaming + image parse + token-cache R/W
counters, RUNBOOK with ports/commands, secrets untracked, stock `/health` still ok.

## Verification
1. `go test ./internal/prismchannel/... -count=1`
2. `go test ./proxy/ -run 'Prism|prism' -count=1` (or equivalent package tests)
3. Build binary; optional side process on 127.0.0.1:8081 with mock prism OR
   in-process tests proving stream/tools/cache without live prism.openai.com
4. `curl -sS http://127.0.0.1:8080/health` still ok (do not break running service
   unless carefully restarting with same env; prefer separate port for experiments)
5. `git status` shows no `.env` / `.api_key` / `.admin_secret` staged
6. Docs: `docs/prism/DESIGN.md` + `docs/prism/RUNBOOK.md`

## Constraints
- Work in `/workspace/codex2api-run` (or sibling clean workdir under /workspace)
- Read reference only from `/workspace/codex-prism` — do not copy secrets/
- Do not invent credentials; mock upstream for tests
- Keep changes coherent; update `.gitignore` if new secret/bin patterns appear
- Prefer additive packages over rewriting main Codex path
- Chinese or English docs OK; code comments English/Chinese OK

## Iteration policy
Research → design (already drafted) → implement → test → fix → optimize TTFT/
concurrency → retest until verification green. If blocked on live Prism cookies,
complete mock path and document live-enable steps.

## Stop when
All verification items pass, or no defensible path remains without live credentials
(then report remaining gaps clearly).
