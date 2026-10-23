# whatsappBot-golang

[![CI](https://github.com/martinezsaweczko/whatsapp-golang/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/martinezsaweczko/whatsapp-golang/actions/workflows/ci.yml)
[![Coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/martinezsaweczko/whatsapp-golang/badges/coverage.json)](https://github.com/martinezsaweczko/whatsapp-golang/actions/workflows/ci.yml)
[![Release](https://github.com/martinezsaweczko/whatsapp-golang/actions/workflows/release.yml/badge.svg)](https://github.com/martinezsaweczko/whatsapp-golang/actions/workflows/release.yml)
[![Latest release](https://img.shields.io/github/v/release/martinezsaweczko/whatsapp-golang)](https://github.com/martinezsaweczko/whatsapp-golang/releases/latest)

WhatsApp bot written in Go (port of the [whatsappBot-node](../whatsappBot-node) Node.js app), built with [whatsmeow](https://go.mau.fi/whatsmeow) — no Chromium, no Puppeteer, native multidevice protocol.

## Features

- **Newspaper kiosk**: `nacional`, `internacional`, `revista` list available files; `periodico: N`, `newspaper: N`, `magazine: N` reply with a one-time download link (RS256 JWT, max 3 uses, 5 min validity)
- **Subscriptions**: `subs: keyword` notifies you (48h link) when a matching file arrives; `del_subs`, `list_subs`
- **Weather**: `tiempo: city` — OpenWeatherMap data rewritten as a friendly note by GPT
- **Weather forecast**: `prevision: city` (or `previsión: city`) — next 24 hours from the OpenWeatherMap 3-hour forecast, summarized as a friendly note by GPT
- **Quote**: `quote` — random quote from API Ninjas with its Spanish translation by GPT
- **TTS**: `audio: text` — replies with a Spanish voice note and deletes your command message
- **Image generation**: `imagen: prompt` — Stability AI
- **Electricity prices**: `electricidad` — PVPC chart from ESIOS (REE), with per-day cache and tomorrow's data after 19:00 Madrid time
- **AI fallback**: mention the bot in a group and GPT answers (text or voice); troll-listed users get trolled
- **Observability**: Prometheus metrics on all layers (HTTP, commands, repository, external APIs) + OTLP traces (Tempo) + `trace_id`-correlated logs

## Architecture

Package-by-layer with consumer-defined interfaces and manual DI (`app/`):

```
cmd/main.go        bootstrap
app/               App builder + wiring
config/            CLI flag configuration with validation
whatsapp/          whatsmeow adapter (Sender iface), command router (regex registry)
services/          kiosk, subscription, weather, quote, tts, ai, imagegen, electricity, jwt
repository/        SQLite (modernc, no CGO), parameterized queries, instrumented decorator
http/              handlers (fileserver, trigger, health, version, metrics) + middleware
o11/               OTel setup, metrics factories, trace-aware slog handler
```

HTTP servers:
- **Public** (`-http-port`, default 46564): `GET /{nacional,internacional,magazine}_folder/{file}?access_token=...` — JWT + replay protection
- **Internal** (`-internal-port`, default 9000): `POST /api/v1/file` (subscription trigger), `POST /api/v1/notifications` (send WhatsApp message), `GET /api/v1/whatsapp/groups`, `GET /api/v1/whatsapp/groups/{jid}/participants`, `GET /api/v1/health`, `GET /api/v1/version`, `GET /metrics`

## Build & run

```bash
make keys              # generate RSA keys for JWT (once)
make swagger           # regenerate Swagger docs from Go annotations
make build             # vet + fmt + tidy + swagger check + build -> build/main
make binary            # regenerate Swagger docs and build without checks or key generation
make test              # go test -race ./...
make coverage          # same suite with a coverage profile (coverage.out); prints the total
make check-swagger     # verify generated Swagger docs are up to date
make migrate           # run goose migrations against local SQLite (default)
make migrate-mysql MYSQL_DSN="user:pass@tcp(localhost:3306)/whatsappbot?parseTime=true"  # run MySQL migrations

## Scheduled commands

The bot can run WhatsApp commands automatically on a cron schedule in a group.
Commands are executed directly by the bot (not sent as a message and re-routed),
so media replies such as `electricidad` charts are posted as plain group messages.

```bash
# Create a schedule (Europe/Madrid timezone by default)
curl -X POST http://localhost:9000/api/v1/scheduler \
  -H 'Content-Type: application/json' \
  -d '{"name":"morning electricity","schedule":"0 8 * * *","command":"electricidad","group_jid":"123456789@g.us"}'

# List schedules
curl http://localhost:9000/api/v1/scheduler

# Update a schedule in place (full replacement; "enabled" defaults to true when omitted)
curl -X PUT http://localhost:9000/api/v1/scheduler/<schedule-uuid> \
  -H 'Content-Type: application/json' \
  -d '{"name":"evening electricity","schedule":"30 20 * * *","command":"electricidad","group_jid":"123456789@g.us","enabled":true}'

# Delete a schedule
curl -X DELETE http://localhost:9000/api/v1/scheduler/<schedule-uuid>
```

`<schedule-uuid>` is the `id` returned by the create and list calls. An update keeps the ID and
`created_at`, and the new schedule takes effect immediately; sending `"enabled":false` stops the
job without deleting it.

Use `-scheduler-timezone` to change the timezone and `-scheduler-enabled=false` to disable the runner.
```

Run (minimal flags):

```bash
./build/main \
  -bot-number 34XXXXXXXXX \
  -bot-notification-jid 34XXXXXXXXX@c.us \
  -nacional-folder /data/nacional/ \
  -internacional-folder /data/internacional/ \
  -magazine-folder /data/magazine/ \
  -db-path /data/db.sqlite \
  -session-db-path /data/session.db \
  -jwt-public-key-path keys/public.pem \
  -jwt-private-key-path keys/private.pem
```

First login shows a QR code in the terminal (or use `-bot-pair-phone 34XXXXXXXXX` for a pairing code). The session is stored in the session DB — subsequent runs connect directly.

### Key flags

| Flag | Default | Description |
|---|---|---|
| `-bot-number` | | Bot phone number (e.g. `34936674253`) |
| `-bot-mentioned-number` | | Bot `@lid` JID for group mentions |
| `-bot-notification-jid` | | JID that receives restart notifications |
| `-bot-troll-numbers` | | Comma-separated JIDs that get the troll AI |
| `-bot-pair-phone` | | Request pairing code instead of QR (first login) |
| `-pdf-channel-jid` | | WhatsApp group/channel JID monitored for automatic PDF downloads |
| `-pdf-category` | `nacional` | Kiosk category where downloaded PDFs are saved |
| `-{nacional,internacional,magazine}-folder` | `/tmp/periodico/...` | Kiosk folders |
| `-retention-{nacional,internacional,magazine}` | `24 / 169 / 720` | Retention (hours) |
| `-url-server` | `localhost:46564` | Public host:port for download links |
| `-http-port` / `-internal-port` | `46564 / 9000` | Server ports |
| `-db-path` / `-session-db-path` | `/tmp/db.sqlite`, `/tmp/session.db` | SQLite paths |
| `-openai-api-key` / `-openai-model` | / `gpt-4o-mini` | OpenAI |
| `-weather-api-key` | | OpenWeatherMap |
| `-quote-api-key` | | API Ninjas (quote command) |
| `-stability-api-key` | | Stability AI |
| `-esios-api-key` | | ESIOS (REE) |
| `-o11-tracer-endpoint` | | OTLP gRPC endpoint (e.g. `tempo:4317`) |
| `-o11-prometheus-path` | `/metrics` | Metrics path on the internal server |
| `-o11-environment` | `dev` | Environment tag in telemetry |

Full list: `./build/main -h`

## Database

The application database (subscriptions, JWT usage, file/user usage) supports SQLite (default) and MySQL.

### SQLite (default)

```bash
./build/main ... -db-driver sqlite -db-path /tmp/db.sqlite ...
```

Migrations are embedded and run automatically on startup. You can also run them manually:

```bash
make migrate                    # uses /tmp/db.sqlite by default
make migrate DB_DSN=/data/db.sqlite
```

### MySQL

The app can create the database and run migrations automatically. You only need a user with `CREATE DATABASE` permission (root works).

#### Quick start with a local MySQL container

```bash
# Start MySQL
podman run -d --name mysql-whatsappbot \
  -e MYSQL_ROOT_PASSWORD=rootpass \
  -p 3306:3306 \
  docker.io/library/mysql:8

# Wait for MySQL to be ready
podman logs -f mysql-whatsappbot
```

#### Run the bot with MySQL (root user, auto-creates DB and tables)

```bash
./build/main \
  -db-driver mysql \
  -db-dsn "root:rootpass@tcp(localhost:3306)/whatsappbot?parseTime=true" \
  -db-auto-create true \
  -session-db-path /tmp/session.db \
  ...
```

#### Using a non-root user

Create the database first, then run with `-db-auto-create false`:

```bash
podman exec mysql-whatsappbot mysql -u root -prootpass -e \
  "CREATE DATABASE IF NOT EXISTS whatsappbot CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci; \
   CREATE USER IF NOT EXISTS 'bot'@'%' IDENTIFIED BY 'botpass'; \
   GRANT ALL PRIVILEGES ON whatsappbot.* TO 'bot'@'%'; \
   FLUSH PRIVILEGES;"

./build/main \
  -db-driver mysql \
  -db-dsn "bot:botpass@tcp(localhost:3306)/whatsappbot?parseTime=true" \
  -db-auto-create false \
  -session-db-path /tmp/session.db \
  ...
```

#### Run migrations manually

```bash
make migrate-mysql MYSQL_DSN="user:pass@tcp(localhost:3306)/whatsappbot?parseTime=true"
```

Note: the WhatsApp session store (`-session-db-path`) still uses SQLite because whatsmeow only supports SQLite and Postgres.

## API documentation

Swagger UI is served on the **internal server** at `/api/v1/docs`. The spec is generated automatically from Go annotations using [swaggo/swag](https://github.com/swaggo/swag):

```bash
make swagger           # regenerate docs from source annotations
# or open http://localhost:9000/api/v1/docs in a browser
```

The UI groups the endpoints as:

- **System**: health, version, metrics
- **Quiosk**: subscription trigger (`/api/v1/file`) and JWT-protected kiosk downloads
- **Whatsapp**: send WhatsApp message (`/api/v1/notifications`), list groups (`/api/v1/whatsapp/groups`), and list group participants (`/api/v1/whatsapp/groups/{jid}/participants`)

### Send a WhatsApp notification

```bash
curl -X POST http://localhost:9000/api/v1/notifications \
  -H 'Content-Type: application/json' \
  -d '{"recipient":"34645568517","message":"Hello from the bot"}'
```

The `recipient` can be an E.164 phone number or a full JID such as `34645568517@c.us` or `120363420531996267@g.us`.

## Container

```bash
make container   # podman build -t whatsappbot-golang:dev
```

```bash
podman run -d --name whatsappbot \
  -p 46564:46564 -p 127.0.0.1:9000:9000 \
  -v ./nacional:/data/nacional:ro \
  -v ./internacional:/data/internacional:ro \
  -v ./magazine:/data/magazine:ro \
  -v ./data/db.sqlite:/data/db.sqlite \
  -v ./data/session.db:/data/session.db \
  -v ./keys:/app/keys:ro \
  whatsappbot-golang:latest \
  -jwt-public-key-path /app/keys/public.pem \
  -jwt-private-key-path /app/keys/private.pem \
  -bot-number 34XXXXXXXXX \
  -bot-notification-jid 34XXXXXXXXX@c.us \
  -nacional-folder /data/nacional/ \
  -internacional-folder /data/internacional/ \
  -magazine-folder /data/magazine/ \
  -db-path /data/db.sqlite \
  -session-db-path /data/session.db
```

## CI/CD

- **CI** (`.github/workflows/ci.yml`): vet, fmt, govulncheck, `go mod tidy` check, Swagger drift check, build, `go test -race` with a coverage profile, container build verification
- **Release** (`.github/workflows/release.yml`): on tag `vX.Y.Z` — validates semver, runs tests, builds and pushes the container to `ghcr.io` with tags `vX.Y.Z`, `X.Y`, `X`, `latest`, and creates a GitHub release

```bash
git tag v0.1.0 && git push origin v0.1.0
```

### Test coverage

The coverage badge is refreshed by CI on every push to `main`: the `coverage-badge` job publishes `coverage.json` to the `badges` branch, which the badge reads. The figure excludes the generated `docs` package, and `make coverage` reproduces it locally.

## Observability

- **Metrics** (Prometheus, internal server): `http_server_requests_total`, `http_server_request_duration_seconds`, `whatsapp_commands_total{command,result}`, `whatsapp_command_duration_seconds`, `repo_queries_total{operation,result}`, `repo_query_duration_seconds`, `external_api_{calls_total,duration_seconds}{service,target,status}` plus business counters (`kiosk_links_generated_total`, `subscription_events_total`, `files_served_total`, `jwt_tokens_rejected_total`, `electricity_cache_total`, `ai_replies_total`, `channelpdf_downloads_total`, `channelpdf_saved_total`, `channelpdf_notify_total`, ...)
- **Traces**: OTLP gRPC to Tempo (`-o11-tracer-endpoint tempo:4317`), spans across HTTP → service → repository; exemplars link metrics to traces
- **Logs**: slog text logs with `trace_id` / `span_id` injected per request/command

## License

See [LICENSE](LICENSE).
