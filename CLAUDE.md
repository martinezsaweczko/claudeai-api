# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

@AGENTS.md

AGENTS.md (imported above) holds the architecture rules, observability requirements, testing conventions and CI/release notes. This file adds only what it does not cover.

## Commands

```bash
go run ./cmd                              # run locally (public :46564, internal :9000)
go test ./http/middleware -run TestWrapped -race -count=1   # single test
make test                                 # go test -race -count=1 ./...
make coverage                             # same suite + coverage.out (docs/ excluded), prints the total
make swagger                              # regenerate docs/ after changing handler annotations
make check-swagger                        # what CI runs: fails if docs/ is stale
make vulncheck                            # govulncheck ./...
make ci                                   # the CI checks locally (all but the container build), without modifying files
```

- `make check` runs `go fmt ./...` and `go mod tidy`, so it modifies files.
- The Go version has one source of truth, the `go` directive in `go.mod`: both workflows read it (`go-version-file`).
- Tooling is not pinned beyond `swag` (`tools/tools.go`); `govulncheck` must be installed separately. There is no golangci-lint config.

## Architecture

### Startup and shutdown

`cmd/main.go` builds the leaf dependencies in order: `config.LoadConfig` -> slog handler wrapped in `o11.NewTraceLogHandler` -> `o11.SetupOTelSDK` -> public and internal `http_server.HTTPServer`. `App.Run` (`app/app.go`) calls `initAndWire`, starts both servers (a bind failure aborts startup) and blocks on SIGINT/SIGTERM; shutdown stops both servers with a 30s timeout.

### Two HTTP servers

- **Public** (`-http-*`, default `:46564`): the OpenAI-compatible API. Write timeout defaults to 0 (disabled) so SSE streams are not cut; read and idle timeouts still apply.
- **Internal** (default `:9000`, **no authentication**, base path `/api/v1`): health, version, Swagger docs and Prometheus metrics. It uses a fixed 30s write timeout.

### Middleware order

The first middleware listed in `middleware.Chain` is the outermost: tracing -> metrics -> logging -> API-key auth (`middleware.APIKeyAuth`, a pass-through when `CLAUDEAI_API_KEYS` is unset) -> handler. The response-writer wrapper shared by logging and metrics implements `Flush` and `Unwrap`.

### Claude configuration and secrets

Flags: `-claude-bin` (`claude`), `-claude-model` (`sonnet`), `-claude-models` (`sonnet,opus,haiku`, advertised by `GET /v1/models`), `-claude-timeout` (`5m`), `-claude-max-concurrency` (`4`, >= 1). Secrets use environment variables, not flags: `CLAUDE_CODE_OAUTH_TOKEN` (optional) and `CLAUDEAI_API_KEYS` (optional, comma-separated; unset means no client auth). `initAndWire` builds `app.apiKeyAuth` and logs `api_key_auth=enabled|disabled` plus warnings (unauthenticated on a non-loopback `-http-address`; `ANTHROPIC_API_KEY` set in the environment). The `/v1` routes (`GET /v1/models`, `GET /v1/models/{model}`, `POST /v1/chat/completions`, public server only) attach tracing -> metrics -> logging -> `app.apiKeyAuth`; see the API section of the README for fields, errors and examples.

Streaming: `POST /v1/chat/completions` with `stream: true` returns OpenAI `chat.completion.chunk` SSE (`http/handlers/chat_stream.go`): role chunk, one chunk per delta, finish chunk (`stop`/`length`), optional usage chunk when `stream_options.include_usage`, then `data: [DONE]`. Errors before the first delta are JSON (same mapping as non-streaming); after it, one SSE error event then `[DONE]`. Flush per event; client disconnect kills the CLI. Try it with `curl -N`; behind a proxy disable buffering and raise the read timeout (no heartbeat). Rejected as 400: tools/functions, `n` > 1, non-text parts, trailing body data.

Backend: `services/claude.CLIBackend` (built in `initAndWire` as `app.claude`) runs `claude -p --output-format stream-json` per request with config isolation flags (`--setting-sources ""`, `--strict-mcp-config`, `--disable-slash-commands`); see the package doc in `services/claude/doc.go` for the verified event shapes and flags.

Skills: `services/skills` (config `-skills-repo`, `-skills-ref`, `-skills-dir`, `-skills-refresh`, `-skills-ssh-key`, `-skills-known-hosts`, `-skills-sync-timeout`) syncs a private skills repo through the `git` binary and exposes `Syncer.Current().Dir` as the CLI cwd for project skills; not wired into `services/claude` yet. See `services/skills/doc.go` for the verified flags (`--tools Skill --setting-sources project`, no `--disable-slash-commands`).
