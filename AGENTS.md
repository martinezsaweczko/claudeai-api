# AGENTS.md

Guidance for coding agents working on this repository.

## What this is

`claudeai-api`: an OpenAI-compatible HTTP API (`/v1/models`, `/v1/chat/completions`, including SSE streaming) backed by the Claude Code CLI. The service is **stateless**: the client sends the full conversation on every request and each request spawns a fresh `claude` process. Built on the go-api-template (stdlib `net/http`, flag config, hand-wired DI, OTel observability). Personal use only.

## Build, test, verify

```bash
go build ./...   # compile
go vet ./...     # static analysis
gofmt -l .       # must output nothing
go test ./...    # unit tests (use -race for CI parity)
make container   # podman image build
```

Before committing, ALL of: `go build ./...`, `go vet ./...`, `gofmt -l .` empty, `go test ./...` green.

## Architecture rules (do not break)

- **Package-by-layer**: `http/handlers` -> `services/*` -> `model`. Never import upwards.
- **Consumer-defined interfaces**: interfaces live where they are *consumed* (for example `app.Server`, `handlers.Router`). External backends (the Claude CLI, later possibly an API-key backend) sit behind an interface owned by the consumer.
- **DI**: config struct + `NewXxx(cfg)` constructors; all wiring centralized in `app/handlers.go` (`initAndWire`). No DI frameworks, no global singletons.
- **Config**: CLI flags (secrets will come from env vars), each section validates itself, errors aggregated in `ConfigError`. Add new flags in `config/config.go`, validation in the section's file.
- **Streaming**: any `http.ResponseWriter` wrapper must implement `Flush()` and `Unwrap()` (see `http/middleware/logging.go`). The public server write timeout defaults to 0 so long SSE responses are not cut; do not reintroduce a fixed write timeout there.
- **Server start**: `HTTPServer.Start` binds synchronously and returns bind errors; serve errors after binding are logged.

## Observability (mandatory for new code)

- **Metrics**: instruments created once via `o11/metrics.go` factories or `o11.NewBusinessCounter` (name pattern: `<area>_<noun>_<verb>_total`). HTTP metrics via `middleware.MetricsMiddleware`. Meter names are `claudeai-api/{http,repository,service}`.
- **Tracing**: child spans per public service operation via the injected `trace.TracerProvider`; record errors with `span.RecordError`. External HTTP calls MUST use `services.NewExternalClient(name, timeout, serviceMetrics)`.
- **Logs**: `log/slog` with contextual attributes; trace IDs are auto-injected by `o11.NewTraceLogHandler` (wired in `cmd/main.go`).
- **Repository pattern note**: if a persistence layer is added later, wrap it in an instrumented decorator that adds spans and `o11.RepoMetrics` per operation (the template had `repository.InstrumentedDB` for this).

## Testing conventions

- Stdlib `testing` only (no testify), table-driven tests, `t.TempDir()` for filesystem, `httptest` for HTTP, fakes implementing the consumer interfaces.
- Use `noop.NewMeterProvider()` and `tracenoop.NewTracerProvider()` for telemetry deps in tests.
- No tests requiring a real `claude` binary or credentials: the CLI must be injectable (fake executable).

## CI/CD

- CI (`.github/workflows/ci.yml`) runs on push/PR: vet, fmt, govulncheck, tidy check, build, `go test -race` through `make coverage` (writes `coverage.out` without the generated `docs` package and reports the total in the job summary), container build. On pushes to `main`, the `coverage-badge` job publishes `coverage.json` to the orphan `badges` branch for the README badge.
- `make ci` runs the CI checks locally (all but the container build) without modifying files. The Go version comes from the `go` directive in `go.mod` in every workflow; `govulncheck` also runs weekly on `main` (`.github/workflows/vulncheck.yml`).
- Release (`.github/workflows/release.yml`) triggers on semver tags `vX.Y.Z`: pushes the container to `ghcr.io/<owner>/<repo>` with `vX.Y.Z / X.Y / X / latest` tags and creates a GitHub release. Tag releases with `git tag vX.Y.Z` only.

## Operational notes

- Health check: `claudeai-api -health-check [port]` (used by the Containerfile HEALTHCHECK; port defaults to 9000).
- The internal server (health, version, metrics, Swagger) has no authentication and must not be exposed publicly.
