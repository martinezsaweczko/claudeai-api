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
make container   # podman image build (--format docker keeps HEALTHCHECK); CLAUDE_VERSION=x.y.z re-pins the CLI
```

Before committing, ALL of: `go build ./...`, `go vet ./...`, `gofmt -l .` empty, `go test ./...` green.

## Architecture rules (do not break)

- **Package-by-layer**: `http/handlers` -> `services/*` -> `model`. Never import upwards.
- **Consumer-defined interfaces**: interfaces live where they are *consumed* (for example `app.Server`, `handlers.Router`). External backends (the Claude CLI, later possibly an API-key backend) sit behind an interface owned by the consumer.
- **DI**: config struct + `NewXxx(cfg)` constructors; all wiring centralized in `app/handlers.go` (`initAndWire`). No DI frameworks, no global singletons.
- **Config**: CLI flags, each section validates itself, errors aggregated in `ConfigError`. Add new flags in `config/config.go`, validation in the section's file. **Secrets are the deliberate exception to flags-only config: they come from environment variables** (flags are visible in process lists): `CLAUDE_CODE_OAUTH_TOKEN` (optional; the CLI falls back to its stored login) and `CLAUDEAI_API_KEYS` (optional, comma-separated client keys). `config.LoadConfigFromEnv` takes an injectable lookup for tests. `ClaudeConfig` implements `slog.LogValuer`/`String()`; never log secret values, only whether they are set.
- **Auth**: `middleware.APIKeyAuth(keys)` is optional Bearer API-key auth. No keys configured means auth is OFF (pass-through); it guards `/v1/*`, `/metrics` and the skills status/refresh routes on the single server (wired in `app/handlers.go`), never health/version/Swagger (public). Errors use the OpenAI shape via `middleware.WriteOpenAIError` (middleware must not import `http/handlers`).
- **Streaming**: any `http.ResponseWriter` wrapper must implement `Flush()` and `Unwrap()` (see `http/middleware/logging.go`). The server write timeout defaults to 0 so long SSE responses are not cut; do not reintroduce a fixed write timeout there.
- **Server start**: `HTTPServer.Start` binds synchronously and returns bind errors; serve errors after binding are logged.
- **Claude backend**: `services/claude` defines `Backend` (`Stream`/`Complete`) and `CLIBackend`, which spawns one isolated `claude -p` per request (prompt on stdin, own process group killed on timeout, `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN` stripped, isolation flags in `buildArgs`). Spike findings and event shapes are in the package doc. Tests use a re-exec fake CLI; the real-CLI test needs `CLAUDEAI_INTEGRATION=1`. `initAndWire` keeps it on `app.claude`.
- **Skills**: `services/skills.Syncer` keeps a bare git cache plus read-only `versions/<sha>/.claude/skills/<name>/` copies of ONLY validated skills (marketplace layout, symlinks/oversized/duplicate skills skipped with a reason; hooks, MCP, agents and commands are never copied). `Current()` returns the atomic `Snapshot` whose `Dir` is the CLI cwd. Config in `config/skills.go` (`-skills-*`, disabled when `-skills-repo` is empty; paths only, no embedded URL credentials). Spike findings (why not `--plugin-dir`: plugin hooks execute) are in `services/skills/doc.go`. Wiring (T6b.2): `app/skills.go` owns the syncer lifecycle (bounded initial sync, `Run` goroutine stopped at shutdown), `claude.WithSkills` takes a function so `services/claude` does not import `services/skills`, and `http/handlers/skills.go` serves `GET /api/v1/skills` and `POST /api/v1/skills/refresh` (protected by the API key when keys are set). With skills active the CLI uses `--tools Skill --setting-sources project` and cwd = snapshot dir; otherwise flags are unchanged. Real-repo test: `CLAUDEAI_SKILLS_E2E=1` with `CLAUDEAI_SKILLS_E2E_REPO/_KEY/_KNOWN_HOSTS`.

- **SSE streaming**: `stream: true` commits the response lazily on the first delta (`sseStream`), so errors before it are JSON with the normal mapping; later errors are one SSE `error` event then `[DONE]`. Flush after every event via `http.NewResponseController`; flush support is probed up front (walking `Unwrap`) because flushing commits headers. The write deadline is cleared per stream, `emit` fails once `r.Context()` is done so the CLI is killed, and there is no heartbeat goroutine. Chunks carry the requested model, not the CLI-resolved one.
- **OpenAI API**: `http/handlers/chat.go` (`POST /v1/chat/completions`, JSON or SSE; SSE lives in `chat_stream.go`) and `models.go` (`GET /v1/models[/{model}]`) depend on the consumer interface `handlers.ChatBackend` and are mounted on the single server in `initAndWire` behind tracing -> metrics -> logging -> `app.apiKeyAuth`. Backend errors map to OpenAI errors in `ChatHandler.mapBackendError` (shared by the JSON path via `fail` and the SSE error event) (fixed messages, never raw CLI output; client cancel writes no body, status 499). Request fields: used `model`/`messages`/`stream`/`stream_options.include_usage`; tools, `n` > 1 and non-text parts are 400, as is trailing data after the body; sampling fields are ignored. Advertised models come from `-claude-models`. Register routes with method patterns (`GET /v1/models`) because metric route labels come from `r.Pattern`.

## Observability (mandatory for new code)

- **Metrics**: instruments created once via `o11/metrics.go` / `o11/metrics_claude.go` factories or `o11.NewBusinessCounter` (name pattern: `<area>_<noun>_<verb>_total`). HTTP metrics via `middleware.MetricsMiddleware`. Meter names are `claudeai-api/{http,repository,service,claude,skills}`. See the Metrics section below.
- **Tracing**: child spans per service operation via the injected `trace.TracerProvider`; record errors with `span.RecordError`. External HTTP calls MUST use `services.NewExternalClient(name, timeout, serviceMetrics)`.
- **Logs**: `log/slog` with contextual attributes; trace IDs are auto-injected by `o11.NewTraceLogHandler` (wired in `cmd/main.go`).
- **Repository pattern note**: if a persistence layer is added later, wrap it in an instrumented decorator that adds spans and `o11.RepoMetrics` per operation (the template had `repository.InstrumentedDB` for this).

## Metrics (T7)

- **Web access (T10)**: `-claude-web=none|search|full` (default `search`; `config.WebNone/WebSearch/WebFull`, unset means none). `services/claude/render.go` `buildArgs(model, system, skills, web)` builds `--tools` from Skill + WebSearch (search|full) + WebFetch (full) and adds `--allowedTools <web tools>` (required: `--tools` alone is denied headless, see the spike in `services/claude/doc.go`); `composeSystem` appends `skillsHint` then `webHint`. With `none` the argument list is byte-for-byte the pre-web one. The parser ignores web tool blocks and counts uses (max of `modelUsage.webSearchRequests`, `usage.server_tool_use` and tool_use blocks; the real CLI leaves `server_tool_use` at 0), feeding `claude_web_requests_total{type=search|fetch}` and the `claude.web.*` span attributes. `full` logs a startup WARN (WebFetch runs on this host, SSRF). A skill (`web-research` in the skills repo) only guides behavior; the flag grants the capability.
- **Catalog and names are a contract** with the Grafana dashboards: `claude_runs_total{model,outcome}`, `claude_run_duration_seconds{model}`, `claude_time_to_first_token_seconds{model}`, `claude_queue_wait_seconds{model}`, `claude_runs_in_flight`, `claude_tokens_total{model,type}`, `claude_cost_usd_total{model}`, `claude_skill_invocations_total{skill}`, `claude_web_requests_total{type}`, `skills_sync_total{outcome}`, `skills_sync_duration_seconds`, `skills_loaded`, `skills_skipped`, `skills_sync_consecutive_failures`, `skills_last_success_timestamp_seconds`, `skills_info{commit,ref}`. The full table (types, label values, token and cost semantics) is in the README "Metrics" section; do not rename without updating `o11/metrics_claude_test.go` (exposition test) and the dashboards.
- **Where**: factories `o11.NewClaudeMetrics` / `o11.NewSkillsMetrics` (meters `claudeai-api/claude`, `claudeai-api/skills`); adapters from `claude.RunInfo` / `skills.SyncInfo` in `app/metrics.go`, wired in `initAndWire` through `claude.WithObserver` / `skills.WithObserver`. Gauges (`claude_runs_in_flight`, `skills_*`) are observable callbacks read at scrape time (`CLIBackend.InFlight`, `Syncer.Status/Current`).
- **Cardinality rule**: labels must be bounded. The `model` label is the CLI-resolved id, else the requested model only if advertised (`-claude-models` or the default), else `other`; `skill` is a loaded skill name or `other`; `outcome`/`type` are fixed enums. Never put a client-controlled string in a label (span attributes may hold it).
- **Dashboard and alerts**: `grafana/dashboard.json` and `grafana/alerts.example.yml` build on these names. When adding or renaming a metric or label, update the dashboard (and the alerts if relevant) and keep `go test ./grafana/` green; it scrapes the real instruments and fails on any unexposed `claude_*`/`skills_*` name or label. Non-app metrics (`http_server_*`, `external_api_*`, `go_*`, `process_*`, `up`) are allowlisted in the test.
- **Observers** must be cheap, non-blocking and panic-safe (the backend and the syncer recover), and the backend and syncer must work with no observer. Instrument names: counters end in `_total` and carry no unit, histograms use unit `s` and end in `_seconds`, so the Prometheus exporter adds no second suffix.
- **Traces**: server span `POST /v1/chat/completions` -> child `claude.run` (attributes `claude.queue_wait_ms`, `claude.ttft_ms`, `claude.skills.{active,commit,invoked}`, tokens, cost; event `first_token`), `skills.sync` separately. Log calls on request, run and sync paths use the `*Context` slog variants so `trace_id`/`span_id` are injected; never log prompts, message content or key paths.
- **CLI telemetry**: the child inherits the gateway environment, so `CLAUDE_CODE_ENABLE_TELEMETRY=1` plus `OTEL_*` on the gateway enables the CLI's own export (see the README); no code is involved.

## Testing conventions

- Stdlib `testing` only (no testify), table-driven tests, `t.TempDir()` for filesystem, `httptest` for HTTP, fakes implementing the consumer interfaces.
- Use `noop.NewMeterProvider()` and `tracenoop.NewTracerProvider()` for telemetry deps in tests.
- No tests requiring a real `claude` binary or credentials: the CLI must be injectable (fake executable).

## CI/CD

- CI (`.github/workflows/ci.yml`) runs on push/PR: vet, fmt, govulncheck, tidy check, build, `go test -race` through `make coverage` (writes `coverage.out` without the generated `docs` package and reports the total in the job summary), container build plus a smoke test of `claude`, `git`, `ssh` inside the image. On pushes to `main`, the `coverage-badge` job publishes `coverage.json` to the orphan `badges` branch for the README badge.
- `make ci` runs the CI checks locally (all but the container build) without modifying files. The Go version comes from the `go` directive in `go.mod` in every workflow; `govulncheck` also runs weekly on `main` (`.github/workflows/vulncheck.yml`).
- Release (`.github/workflows/release.yml`) triggers on semver tags `vX.Y.Z`: pushes the container to `ghcr.io/<owner>/<repo>` with `vX.Y.Z / X.Y / X / latest` tags and creates a GitHub release. Tag releases with `git tag vX.Y.Z` only.

## Operational notes

- Health check: `claudeai-api -health-check [port]` (used by the Containerfile HEALTHCHECK; port defaults to 46564, the single port; health is public).
- **Secure-default bind**: `-http-address` defaults to `127.0.0.1`. The container CMD passes `0.0.0.0` explicitly; publish the port on `127.0.0.1` only (or set `CLAUDEAI_API_KEYS`). Do not change the defaults back to `0.0.0.0` (`config/load_test.go` asserts them).
- **Container** (`Containerfile`): 3 stages (verified Claude Code CLI download, Go build, `debian:bookworm-slim` runtime with `git` + `openssh-client`), user `appuser` uid/gid 1001, `HOME=/home/appuser`, skills state `/var/lib/claudeai-api`, secrets mounted read-only under `/run/secrets/`. The CLI version is pinned by `ARG CLAUDE_VERSION` and checked against Anthropic's GPG-signed manifest (key fingerprint pinned in the file). The builder image tag (`golang:1.27.3-bookworm`) must satisfy the `go` directive in `go.mod`. `compose.example.yaml` and the README "Quickstart B" hold the verified run commands (rootless podman needs `--userns=keep-id:uid=1001,gid=1001` for the deploy key).
- `make check-swagger` runs swag from the version pinned in `go.mod` (`go run`) and gofmt-formats `docs.go` before diffing; use `make swagger` to regenerate.
- There is ONE server and ONE port: `/metrics` and skills refresh share the API port, so without `CLAUDEAI_API_KEYS` they are open; keep the bind on loopback. Do not reintroduce a second server or `-internal-*` flags.
