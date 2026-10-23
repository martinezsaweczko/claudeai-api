# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

@AGENTS.md

AGENTS.md (imported above) holds the architecture rules, observability requirements, testing conventions and CI/release notes. This file adds only what it does not cover, plus corrections where the code has moved on.

## Commands

```bash
go run ./cmd -bot-number <number> -bot-notification-jid <jid>   # run locally (the only two required flags)
go test ./whatsapp -run TestFirstMatchWins -race -count=1        # single test
go test ./services/kiosk/...                                     # single package
make test                                                        # go test -race -count=1 ./...
make coverage                                                    # same suite + coverage.out (docs/ excluded), prints the total
make coverage-total                                              # bare number from an existing coverage.out
make swagger                                                     # regenerate docs/ after changing handler annotations
make check-swagger                                               # what CI runs: fails if docs/ is stale
make vulncheck                                                   # govulncheck ./...
make ci                                                          # the CI checks locally (all but the container build), without modifying files
```

Makefile targets that do more than their name suggests:

- `make build` depends on `keys`, which depends on `clean-keys`: it deletes and regenerates `keys/` every time, invalidating all issued download links. Use `go build ./...` or `make binary` to keep existing keys.
- `make check` runs `go fmt ./...` and `go mod tidy`, so it modifies files.
- `make run` starts the binary with no flags and fails config validation; use the `go run` line above.
- `make binary` creates `/tmp/periodico/{nacional,internacional,magazine}`, the default kiosk folders.

Running locally needs `keys/public.pem` and `keys/private.pem` (`make keys`). Defaults: public server on `:46564`, internal server on `:9000`. On first run (no session in `-session-db-path`) the client prints a QR code to stdout, or requests a pairing code when a pairing phone is configured.

The Go version has one source of truth, the `go` directive in `go.mod`: both workflows read it (`go-version-file`), and a local older Go downloads that toolchain automatically. `govulncheck` also runs weekly on `main` (`.github/workflows/vulncheck.yml`) because new advisories can break CI without any commit; the usual fix is bumping the `go` directive or the flagged module.

Tooling is not pinned beyond `swag` (`tools/tools.go`): the `goose` and `govulncheck` CLIs must be installed separately. There is no golangci-lint config.

### Migrations

- SQL migrations are per driver: `repository/migrations/{sqlite,mysql}/NNN_*.sql`. A schema change needs a file in both directories.
- Go migrations live in `repository/migrations/*.go`, are shared by both drivers (they detect the dialect at runtime) and are registered by blank import in `repository/repository.go`.
- Everything is embedded and applied by `goose.Up` at startup inside `repository.New`. `make migrate` / `make migrate-mysql` use the goose CLI against the SQL directory only, so they do not run Go migrations.

## Architecture

### Startup and shutdown

`cmd/main.go` builds the leaf dependencies in order: `config.LoadConfig` → slog handler wrapped in `o11.NewTraceLogHandler` → `o11.SetupOTelSDK` → `repository.New` (opens the DB and migrates) → `whatsapp.NewClient` → `whatsapp.NewRouter`, then connects them with `waClient.AddEventHandler(router.HandleEvent)`.

`App.Run` (`app/app.go`) calls `initAndWire` (services, HTTP routes, command registration), starts the public and internal HTTP servers, connects WhatsApp, starts the scheduler and blocks on SIGINT/SIGTERM. Shutdown stops the servers (30s), then the scheduler, then waits for in-flight command handlers (`router.Wait()`, unbounded), then disconnects WhatsApp.

`HTTPServer.Start` listens in a goroutine and always returns nil: a port already in use is only logged, the process keeps running.

### Incoming message path

`Router.HandleEvent` → `dispatch` (`whatsapp/router.go`):

1. Messages from the bot itself are dropped.
2. A document posted in the `-pdf-channel-jid` chat goes to the channel-media handler (`services/channelpdf`) before any command matching.
3. The first command whose `Pattern` matches the body wins. Order is the registration order in `initAndWire`: kiosk, subscription, tts, weather, imagegen, electricity. Patterns are anchored prefixes, so a new command can be shadowed by an earlier, shorter one.
4. With no match, the AI fallback (`services/ai`) runs only if the bot is mentioned. The mention check is an exact string compare against `-bot-mentioned-number` and `<bot-number>@c.us`.

`Router.execute` runs the matched handler in a goroutine with panic recovery, a `-command-timeout` context (default 60s), a `whatsapp.command.<name>` span and command metrics.

### Scheduled command path

The scheduler (`services/scheduler`, robfig/cron, 5-field expressions, only when `-scheduler-enabled`) calls `Router.ExecuteCommand` with a synthetic `IncomingMessage`. This path matches patterns the same way but calls the handler **synchronously and bypasses `execute`**: no timeout, no panic recovery, no command metrics, no WaitGroup tracking, and no AI fallback. Handlers reachable from the scheduler must be safe under those conditions. Jobs are skipped while WhatsApp is disconnected.

### Two HTTP servers

- **Public** (`-http-*`, default `:46564`): only `GET /{nacional,internacional,magazine}_folder/{file}`, behind tracing → metrics → logging → JWT (the first middleware listed is the outermost).
- **Internal** (default `:9000`, **no authentication**, base path `/api/v1`): health, version, Swagger docs, Prometheus metrics, `POST /file` (triggers subscription notifications), `POST /notifications`, WhatsApp group listing, and `/scheduler` CRUD. It must not be exposed publicly. `/health` always returns ok and does not reflect the WhatsApp connection state.

### Download tokens

`http/middleware/jwt_query.go` reads `?access_token=`, verifies signature and expiry, then counts prior uses in the `jwt_used` table and records this one. Tokens are not bound to a file or category (constant subject), and a use is consumed even when the file turns out not to exist. Kiosk file indices shown to users are positions in a listing recomputed on every request (files within the retention window, newest first), not stable IDs.

### Persistence

- `repository.DB` holds the queries; `repository.InstrumentedDB` embeds it and adds spans and metrics. A new repository method is only instrumented if a wrapper is added in `instrumented.go`; otherwise the embedded method is promoted silently.
- Tables: `subscriptions`, `jwt_used`, `file_usage`, `user_usage`, `scheduled_commands`.
- Primary keys are UUID v7, stored as TEXT on SQLite and BINARY(16) on MySQL; go through `uuidValue` / `scanUUID`. Dialect-specific SQL branches on `DB.dialect`.
- SQLite runs with a single open connection.

### Service behaviors worth knowing

- **electricity**: the cache key uses the UTC date, while the "tomorrow's prices" window is 19:00–22:59 Europe/Madrid. A failed fetch writes a marker that blocks refetching for the rest of that day. Charts are drawn by hand with `fogleman/gg` and embedded fonts (no system font dependency).
- **kiosk**: usage statistics are keyed by the sender's push name, not their JID.
- **subscription**: a subscription matches when its text is a case-insensitive substring of the new file name.
- **channelpdf**: saves any document from the configured channel (no MIME check); an already existing file skips both the save and the notification.
- **tts**: uses the unofficial Google Translate TTS endpoint, Spanish only.

## Where AGENTS.md is out of date

- "Handlers run in goroutines with timeout + recover" holds for real messages only, not for the scheduled path above.
- `go.mau.fi/whatsmeow/types` is imported beyond `whatsapp` and `app` (config, HTTP handlers, several services). The rule to keep is: the whatsmeow client and events stay inside `whatsapp`.
- CI also installs `swag` and runs `make check-swagger`, and builds the container with Docker rather than podman.
- CI runs the tests through `make coverage`, not `make test`. On a push to `main` the `coverage-badge` job force-pushes `coverage.json` as a single commit to the orphan `badges` branch (the only job with `contents: write`); the README badge reads that file.
- `.github/.copilot-instructions` describes the original template (different module, CGO SQLite driver, env-var config) and should be ignored.
