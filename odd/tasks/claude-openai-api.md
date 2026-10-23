# claude-openai-api

## Objective
OpenAI-compatible HTTP API (`/v1/models`, `/v1/chat/completions` incl. SSE) that uses the Claude Code CLI authenticated with the owner's Claude Max subscription. Personal use only. Built from the go-api-template baseline.

## Decisions
- Stateless: client sends full history; each request spawns a fresh `claude -p --output-format stream-json --include-partial-messages --tools "" --no-session-persistence`. Any container serves any request.
- Auth: `claude setup-token` once -> `CLAUDE_CODE_OAUTH_TOKEN` secret per container; service unsets `ANTHROPIC_API_KEY`. Never `--bare` (ignores OAuth token).
- Claude backend behind an interface so an API-key backend can replace the CLI later.
- Skills live in a separate repo (Claude Code plugin marketplace); service syncs a pinned ref and passes it via `--plugin-dir`. Remote repo creation needs explicit user confirmation.
- Secrets via env vars (deviation from template's flags-only config); other config stays flags.
- Policy note: Anthropic terms forbid routing Max credentials on behalf of other users; personal use only, concurrency-limited.

## Scope
Allowed: everything in this repository. Not allowed without asking: creating remote GitHub repos, push, PRs.

## Constraints
- Go: stdlib testing only (no testify), table-driven, httptest, consumer-side interfaces, hand-wired DI.
- Conventional commits, NO Co-Authored-By / AI attribution (user rule).
- ~400 authored changed lines per task is a planning heuristic only.
- Test-first where a runnable deterministic test exists.

## Tasks
- [ ] T1 Strip WhatsApp domain; rename module/meters/app; fix streaming blockers (Flusher/Unwrap on response wrapper, WriteTimeout, Start() swallowing bind errors)
- [ ] T2 Config: Claude section, env-var secrets, Bearer API-key middleware
- [ ] T3 Claude backend interface + CLI implementation (stream-json parsing, errors, fake CLI tests)
- [ ] T4 `/v1/models` + non-streaming `/v1/chat/completions`
- [ ] T5 SSE streaming chunks
- [ ] T6a Create skills repo (plugin marketplace layout, example skill, README) locally
- [ ] T6b Service-side skills sync (pinned ref, refresh timer/webhook, `--plugin-dir`, version metric)
- [ ] T7 Metrics + traces (requests, latency, TTFT, tokens, cost, process errors, skills version; spans; Grafana)
- [ ] T8 Containerfile with `claude` binary, CI cleanup, README

## Acceptance
`go build ./... && go vet ./... && gofmt -l . && go test -race ./...` clean after each task.

## Progress / evidence
(route per task, commits, check results recorded below)

## Delivery
Strategy: ask-on-risk (default). Branch: feat/claude-openai-api. Baseline commit c1675a2.
