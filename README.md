# claudeai-api

[![CI](https://github.com/martinezsaweczko/claudeai-api/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/martinezsaweczko/claudeai-api/actions/workflows/ci.yml)
[![Coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/martinezsaweczko/claudeai-api/badges/coverage.json)](https://github.com/martinezsaweczko/claudeai-api/actions/workflows/ci.yml)
[![Release](https://github.com/martinezsaweczko/claudeai-api/actions/workflows/release.yml/badge.svg)](https://github.com/martinezsaweczko/claudeai-api/actions/workflows/release.yml)
[![Latest release](https://img.shields.io/github/v/release/martinezsaweczko/claudeai-api)](https://github.com/martinezsaweczko/claudeai-api/releases/latest)

OpenAI-compatible HTTP API (`/v1/models`, `/v1/chat/completions`, including SSE streaming) backed by the Claude Code CLI. The service is stateless: the client sends the full conversation on every request and each request runs a fresh `claude` process. Personal use only.

> Status: the repository currently contains the generic Go service skeleton (config, HTTP servers, middleware, observability). The OpenAI-compatible endpoints are being added.

## Architecture

Package-by-layer with consumer-defined interfaces and manual DI (`app/`):

```
cmd/main.go        bootstrap
app/               App builder + wiring
config/            CLI flag configuration with validation
http_server/       net/http server wrapper (synchronous bind, graceful stop)
http/              handlers (health, version, metrics, swagger) + middleware
services/          shared service helpers (instrumented HTTP client, JWT)
o11/               OTel setup, metrics factories, trace-aware slog handler
```

HTTP servers:
- **Public** (`-http-port`, default 46564): the API. The write timeout (`-http-write-timeout`) defaults to 0 (disabled) so long SSE streams are not cut.
- **Internal** (`-internal-port`, default 9000): `GET /api/v1/health`, `GET /api/v1/version`, `GET /api/v1/docs` (Swagger UI), `GET /metrics`. No authentication: do not expose it publicly.

## Build & run

```bash
make swagger           # regenerate Swagger docs from Go annotations
make build             # vet + fmt + tidy + swagger check + build -> build/main
make binary            # regenerate Swagger docs and build without checks
make test              # go test -race ./...
make coverage          # same suite with a coverage profile (coverage.out); prints the total
make check-swagger     # verify generated Swagger docs are up to date
./build/main           # run with defaults
```

### Key flags

| Flag | Default | Description |
|---|---|---|
| `-http-port` / `-internal-port` | `46564 / 9000` | Server ports |
| `-http-read-timeout` | `10` | Read timeout in seconds |
| `-http-write-timeout` | `0` | Public write timeout in seconds (0 disables it, needed for SSE) |
| `-http-timeout` | `30` | Idle timeout in seconds |
| `-log-level` / `-log-file-path` | `info / stdout` | Logging |
| `-o11-tracer-endpoint` | | OTLP gRPC endpoint (e.g. `tempo:4317`) |
| `-o11-prometheus-path` | `/metrics` | Metrics path on the internal server |
| `-o11-environment` | `dev` | Environment tag in telemetry |

Full list: `./build/main -h`

## API documentation

Swagger UI is served on the **internal server** at `/api/v1/docs`. The spec is generated from Go annotations using [swaggo/swag](https://github.com/swaggo/swag) (`make swagger`).

## Container

```bash
make container   # podman build -t claudeai-api:dev
```

## CI/CD

- **CI** (`.github/workflows/ci.yml`): vet, fmt, govulncheck, `go mod tidy` check, Swagger drift check, build, `go test -race` with a coverage profile, container build verification
- **Release** (`.github/workflows/release.yml`): on tag `vX.Y.Z` - validates semver, runs tests, builds and pushes the container to `ghcr.io` with tags `vX.Y.Z`, `X.Y`, `X`, `latest`, and creates a GitHub release

```bash
git tag v0.1.0 && git push origin v0.1.0
```

### Test coverage

The coverage badge is refreshed by CI on every push to `main`: the `coverage-badge` job publishes `coverage.json` to the `badges` branch, which the badge reads. The figure excludes the generated `docs` package, and `make coverage` reproduces it locally.

## Observability

- **Metrics** (Prometheus, internal server): `http_server_requests_total`, `http_server_request_duration_seconds`, `http_server_requests_in_flight`, `repo_queries_total`, `repo_query_duration_seconds`, `external_api_{calls_total,duration_seconds}{service,target,status}` plus business counters
- **Traces**: OTLP gRPC (`-o11-tracer-endpoint tempo:4317`), spans across HTTP -> service
- **Logs**: slog text logs with `trace_id` / `span_id` injected per request

## License

See [LICENSE](LICENSE).
