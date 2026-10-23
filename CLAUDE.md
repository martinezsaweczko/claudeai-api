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

Flags: `-claude-bin` (`claude`), `-claude-model` (`sonnet`), `-claude-timeout` (`5m`), `-claude-max-concurrency` (`4`, >= 1). Secrets use environment variables, not flags: `CLAUDE_CODE_OAUTH_TOKEN` (optional) and `CLAUDEAI_API_KEYS` (optional, comma-separated; unset means no client auth). `initAndWire` builds `app.apiKeyAuth` and logs `api_key_auth=enabled|disabled` plus warnings (unauthenticated on a non-loopback `-http-address`; `ANTHROPIC_API_KEY` set in the environment). The `/v1` routes (added later) attach `app.apiKeyAuth`.
