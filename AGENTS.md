# AGENTS.md

Guidance for coding agents working on this repository.

## What this is

Go port of the whatsappBot-node app: a WhatsApp bot (newspaper kiosk with JWT-protected downloads, subscriptions, weather, TTS, AI image/text, electricity price charts) using **whatsmeow** (`go.mau.fi/whatsmeow`). Based on the go-api-template (stdlib `net/http`, flag config, hand-wired DI, OTel observability).

## Build, test, verify

```bash
make keys        # once: RSA keys into keys/ (gitignored, required to run)
go build ./...   # compile
go vet ./...     # static analysis
gofmt -l .       # must output nothing
go test ./...    # unit tests (use -race for CI parity)
make container   # podman image build
```

Before committing, ALL of: `go build ./...`, `go vet ./...`, `gofmt -l .` empty, `go test ./...` green.

## Architecture rules (do not break)

- **Package-by-layer**: `http/handlers` → `services/*` → `repository` → `model`. Never import upwards.
- **Consumer-defined interfaces**: interfaces live where they are *consumed* (`whatsapp.Sender` in `whatsapp/router.go`, `kiosk.UsageStore`, `middleware.TokenStore`, ...). The whatsmeow library is imported ONLY by the `whatsapp` package and `app` wiring (types.JID in signatures is the accepted exception).
- **DI**: config struct + `NewXxx(cfg)` constructors; all wiring centralized in `app/handlers.go` (`initAndWire`). No DI frameworks, no global singletons (template's `services.JWTService` aside).
- **WhatsApp commands**: registered as `whatsapp.Command{Name, Pattern, Handler}` in each service's `Commands()` method; first regex match wins; the AI fallback runs only on bot mention. Handlers must respect `ctx` cancellation (they run in goroutines with timeout + recover). Scheduled commands reuse the same handlers via `Router.ExecuteCommand` using a synthetic `IncomingMessage`; `Client.ReplyText`/`ReplyMedia` fall back to plain sends for synthetic messages.
- **Config**: CLI flags only (no env files), each section validates itself, errors aggregated in `ConfigError`. Add new flags in `config/config.go`, validation in the section's file.
- **SQL**: parameterized queries only, in `repository/`. Schema compatible with the Node app's `db.sqlite` (tables: `subscriptions`, `jwt_used`, `file_usage`, `user_usage`).
- **Database drivers**: SQLite (`modernc.org/sqlite`, driver name `sqlite`) and MySQL (`github.com/go-sql-driver/mysql`, driver name `mysql`) are supported. Migrations are managed with `goose` and embedded in the binary. The WhatsApp session store (`-session-db-path`) must remain SQLite because whatsmeow's `sqlstore` does not support MySQL.
- **SQLite driver**: `modernc.org/sqlite` (pure Go, driver name `sqlite`) — do NOT add CGO drivers; the container builds with `CGO_ENABLED=0`.

## Observability (mandatory for new code)

- **Metrics**: instruments created once via `o11/metrics.go` factories or `o11.NewBusinessCounter` (name pattern: `<area>_<noun>_<verb>_total`). HTTP metrics via `middleware.MetricsMiddleware`; repo metrics via the `repository.InstrumentedDB` decorator.
- **Tracing**: child spans per public service/repo operation via the injected `trace.TracerProvider`; record errors with `span.RecordError`. External HTTP calls MUST use `services.NewExternalClient(name, timeout, serviceMetrics)`.
- **Logs**: `log/slog` with contextual attributes; trace IDs are auto-injected by `o11.NewTraceLogHandler` (wired in `cmd/main.go`).

## Testing conventions

- Table-driven tests, `t.TempDir()` for filesystem, `httptest` for external APIs, fakes implementing the consumer interfaces (see `services/subscription/subscription_test.go` for the `whatsapp.Sender` fake).
- Use `noop.NewMeterProvider()` and `tracenoop.NewTracerProvider()` for telemetry deps in tests.
- No tests requiring a real WhatsApp connection or real API keys — external endpoints must be injectable/overridable.

## CI/CD

- CI (`.github/workflows/ci.yml`) runs on push/PR: vet, fmt, govulncheck, tidy check, build, `go test -race` through `make coverage` (writes `coverage.out` without the generated `docs` package and reports the total in the job summary), container build. On pushes to `main`, the `coverage-badge` job publishes `coverage.json` to the orphan `badges` branch for the README badge.
- `make ci` runs the CI checks locally (all but the container build) without modifying files. The Go version comes from the `go` directive in `go.mod` in every workflow; `govulncheck` also runs weekly on `main` (`.github/workflows/vulncheck.yml`).
- Release (`.github/workflows/release.yml`) triggers on semver tags `vX.Y.Z`: pushes container to `ghcr.io/<owner>/whatsappbot-golang` with `vX.Y.Z / X.Y / X / latest` tags and creates a GitHub release. Tag releases with `git tag vX.Y.Z` only.

## Operational notes

- JWT keys: `make keys` (RSA 2048). Never commit `keys/` (gitignored). Download links: 5-min tokens for direct requests, 48h for subscriptions; a token is valid while its recorded use count is ≤ 2 (Node parity).
- WhatsApp session lives in `-session-db-path`; deleting it forces a new QR/pairing login. `events.LoggedOut` exits the process (container restarts it).
- The container needs `-jwt-*-key-path` mounted and, for Madrid-time logic, `tzdata` (already in the Containerfile).
- Health check: `whatsappbot -health-check [port]` (used by the Containerfile HEALTHCHECK; port defaults to 9000).
