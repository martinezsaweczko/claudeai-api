# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

@AGENTS.md

AGENTS.md (imported above) holds the architecture rules, observability requirements, testing conventions and CI/release notes. This file adds only what it does not cover.

## Commands

```bash
make run                                  # go run ./cmd, loopback only (public :46564, internal :9000)
go test ./http/middleware -run TestWrapped -race -count=1   # single test
make test                                 # go test -race -count=1 ./...
make coverage                             # same suite + coverage.out (docs/ excluded), prints the total
make swagger                              # regenerate docs/ after changing handler annotations
make check-swagger                        # what CI runs: fails if docs/ is stale
make vulncheck                            # govulncheck ./...
make ci                                   # the CI checks locally (all but the container build), without modifying files
make container                            # podman build --format docker; make container-run uses ./.env
```

- `make check` runs `go fmt ./...` and `go mod tidy`, so it modifies files.
- The Go version has one source of truth, the `go` directive in `go.mod`: both workflows read it (`go-version-file`).
- `make swagger`/`check-swagger` run the `swag` pinned in `go.mod` via `go run`, so CI installs nothing. Tooling is not pinned beyond `swag` (`tools/tools.go`); `govulncheck` must be installed separately. There is no golangci-lint config.

## Architecture

### Startup and shutdown

`cmd/main.go` builds the leaf dependencies in order: `config.LoadConfig` -> slog handler wrapped in `o11.NewTraceLogHandler` -> `o11.SetupOTelSDK` -> public and internal `http_server.HTTPServer`. `App.Run` (`app/app.go`) calls `initAndWire`, starts both servers (a bind failure aborts startup) and blocks on SIGINT/SIGTERM; shutdown stops both servers with a 30s timeout.

### Two HTTP servers

- **Public** (`-http-*`, default `127.0.0.1:46564`; the container passes `0.0.0.0`): the OpenAI-compatible API. Write timeout defaults to 0 (disabled) so SSE streams are not cut; read and idle timeouts still apply.
- **Internal** (default `127.0.0.1:9000`, **no authentication**, base path `/api/v1`): health, version, Swagger docs, Prometheus metrics and the skills status/refresh endpoints. It uses a fixed 30s write timeout.

### Middleware order

The first middleware listed in `middleware.Chain` is the outermost: tracing -> metrics -> logging -> API-key auth (`middleware.APIKeyAuth`, a pass-through when `CLAUDEAI_API_KEYS` is unset) -> handler. The response-writer wrapper shared by logging and metrics implements `Flush` and `Unwrap`.

### Claude configuration and secrets

Flags: `-claude-bin` (`claude`), `-claude-model` (`sonnet`), `-claude-models` (`sonnet,opus,haiku`, advertised by `GET /v1/models`), `-claude-timeout` (`5m`), `-claude-max-concurrency` (`4`, >= 1). Secrets use environment variables, not flags: `CLAUDE_CODE_OAUTH_TOKEN` (optional) and `CLAUDEAI_API_KEYS` (optional, comma-separated; unset means no client auth). `initAndWire` builds `app.apiKeyAuth` and logs `api_key_auth=enabled|disabled` plus warnings (unauthenticated on a non-loopback `-http-address`, which is what the container does: publish on `127.0.0.1` only; `ANTHROPIC_API_KEY` set in the environment). The `/v1` routes (`GET /v1/models`, `GET /v1/models/{model}`, `POST /v1/chat/completions`, public server only) attach tracing -> metrics -> logging -> `app.apiKeyAuth`; see the API section of the README for fields, errors and examples.

Streaming: `POST /v1/chat/completions` with `stream: true` returns OpenAI `chat.completion.chunk` SSE (`http/handlers/chat_stream.go`): role chunk, one chunk per delta, finish chunk (`stop`/`length`), optional usage chunk when `stream_options.include_usage`, then `data: [DONE]`. Errors before the first delta are JSON (same mapping as non-streaming); after it, one SSE error event then `[DONE]`. Flush per event; client disconnect kills the CLI. Try it with `curl -N`; behind a proxy disable buffering and raise the read timeout (no heartbeat). Rejected as 400: tools/functions, `n` > 1, non-text parts, trailing body data.

Backend: `services/claude.CLIBackend` (built in `initAndWire` as `app.claude`) runs `claude -p --output-format stream-json` per request with config isolation flags (`--setting-sources ""`, `--strict-mcp-config`, `--disable-slash-commands`); see the package doc in `services/claude/doc.go` for the verified event shapes and flags.

Skills: `services/skills` (config `-skills-repo`, `-skills-ref`, `-skills-dir`, `-skills-refresh`, `-skills-ssh-key`, `-skills-known-hosts`, `-skills-sync-timeout`) syncs a private skills repo through the `git` binary. `app/skills.go` builds the syncer when `-skills-repo` is set (misconfiguration such as a missing key file fails startup), runs one bounded (20s) initial `Sync` before the servers start (failure only warns), then `Syncer.Run` in a goroutine that shutdown cancels and waits for (5s). `services/claude.WithSkills(func() (dir, ok))` receives the adapted `Current()`; per run it is called once, and when ok the CLI gets `--tools Skill --setting-sources project`, no `--disable-slash-commands`, cwd = snapshot dir and a fixed system-prompt hint (`skillsHint`); otherwise flags are unchanged. `Result.Text` is the text of all assistant turns joined by a blank line (same for Stream and Complete). Internal endpoints (no auth, internal port only): `GET /api/v1/skills` and `POST /api/v1/skills/refresh` (`http/handlers/skills.go`, consumer interface `handlers.SkillsSource`). Shutdown also calls the backend's `Close` (removes its temp cwd). See the README "Skills" section and `services/skills/doc.go` / `services/claude/doc.go`.

### Metrics and traces

Claude and skills metrics live in `o11/metrics_claude.go` (`NewClaudeMetrics`, `NewSkillsMetrics`, meters `claudeai-api/claude` and `claudeai-api/skills`); `app/metrics.go` adapts `claude.RunInfo` and `skills.SyncInfo` to them and `initAndWire` passes the observers through `claude.WithObserver` / `skills.WithObserver`. Catalog: `claude_runs_total{model,outcome}`, `claude_run_duration_seconds{model}`, `claude_time_to_first_token_seconds{model}`, `claude_queue_wait_seconds{model}`, `claude_runs_in_flight`, `claude_tokens_total{model,type}` (prompt side = input + cache_creation + cache_read, summed over all turns), `claude_cost_usd_total{model}` (CLI estimate, not billing), `claude_skill_invocations_total{skill}`, `skills_sync_total{outcome=success|noop|error}`, `skills_sync_duration_seconds`, and scrape-time gauges `skills_loaded`, `skills_skipped`, `skills_sync_consecutive_failures`, `skills_last_success_timestamp_seconds`, `skills_info{commit,ref}` (current snapshot only). The README "Metrics" section has the full table and bounded label values. These names are the Grafana contract; `o11/metrics_claude_test.go` asserts the exact exposed names. `grafana/dashboard.json` and `grafana/alerts.example.yml` use them; when adding or renaming a metric or label, update the dashboard and keep `go test ./grafana/` green (it scrapes the real instruments and fails on unexposed names or labels).

Cardinality rule: label values come from bounded sets. `model` = CLI-resolved id, else the requested model only if advertised (`-claude-models` or the default), else `other`; `skill` = loaded skill name or `other`. Never label with a client string. Observers are cheap and panic-safe; both the backend and the syncer work without one.

Trace shape: `otelhttp` server span `POST /v1/chat/completions` -> child `claude.run` (attributes `claude.model`, `claude.model.resolved`, `claude.outcome`, `claude.queue_wait_ms`, `claude.ttft_ms`, `claude.num_turns`, tokens, cost, `claude.skills.active|commit|invoked`; event `first_token`); `skills.sync` is its own trace. Logs on request/run/sync paths use `*Context` slog calls so `trace_id`/`span_id` appear (`o11.TraceLogHandler`). The CLI's own telemetry can be forwarded without code changes: the child inherits the gateway environment, so set `CLAUDE_CODE_ENABLE_TELEMETRY=1` and `OTEL_*` on the gateway process (CLI traces are beta via `CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1`; keep prompt logging off). It partly duplicates the metrics above.
