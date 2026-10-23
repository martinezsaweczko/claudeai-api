# claudeai-api

[![CI](https://github.com/martinezsaweczko/claudeai-api/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/martinezsaweczko/claudeai-api/actions/workflows/ci.yml)
[![Coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/martinezsaweczko/claudeai-api/badges/coverage.json)](https://github.com/martinezsaweczko/claudeai-api/actions/workflows/ci.yml)
[![Release](https://github.com/martinezsaweczko/claudeai-api/actions/workflows/release.yml/badge.svg)](https://github.com/martinezsaweczko/claudeai-api/actions/workflows/release.yml)
[![Latest release](https://img.shields.io/github/v/release/martinezsaweczko/claudeai-api)](https://github.com/martinezsaweczko/claudeai-api/releases/latest)

OpenAI-compatible HTTP API (`/v1/models`, `/v1/chat/completions`, including SSE streaming) backed by the Claude Code CLI. The service is stateless: the client sends the full conversation on every request and each request runs a fresh `claude` process. Personal use only.

> Status: `GET /v1/models` and non-streaming `POST /v1/chat/completions` are implemented. SSE streaming (`stream: true`) is not available yet and is rejected with `400 streaming_not_supported`.

## Architecture

Package-by-layer with consumer-defined interfaces and manual DI (`app/`):

```
cmd/main.go        bootstrap
app/               App builder + wiring
config/            CLI flag configuration with validation
http_server/       net/http server wrapper (synchronous bind, graceful stop)
http/              handlers (health, version, metrics, swagger) + middleware
services/          shared service helpers (instrumented HTTP client)
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
| `-claude-bin` | `claude` | Claude Code CLI binary name or path |
| `-claude-model` | `sonnet` | Default model alias when a request names none |
| `-claude-models` | `sonnet,opus,haiku` | Comma-separated ids advertised by `GET /v1/models` (no empty entries; duplicates removed; the default model is added if missing). Any model the CLI accepts still works in requests |
| `-claude-timeout` | `5m` | Max wall time per request |
| `-claude-max-concurrency` | `4` | Max simultaneous CLI processes (>= 1) |

Full list: `./build/main -h`

### Secrets and authentication

Secrets are read from environment variables, not flags (flags are visible in process lists). This deliberately deviates from the flags-only convention of the rest of the config.

| Variable | Description |
|---|---|
| `CLAUDE_CODE_OAUTH_TOKEN` | Optional. Token from `claude setup-token`. If unset, the CLI uses its own stored login. Never logged. |
| `CLAUDEAI_API_KEYS` | Optional, comma-separated client API keys. If unset or empty, authentication is **off**. |
| `ANTHROPIC_API_KEY` | Do not set: it overrides the subscription login and bills the API key. The service warns if it is set. |

When keys are configured, the `/v1` API requires `Authorization: Bearer <key>` and answers failures with `401`, `WWW-Authenticate: Bearer` and an OpenAI-style body (`missing_api_key` / `invalid_api_key`). The internal server (health, version, metrics, Swagger) is never authenticated. With auth disabled and a non-loopback `-http-address` the service logs a warning at startup; for local-only use pass `-http-address 127.0.0.1`.

```bash
# auth disabled (no CLAUDEAI_API_KEYS)
curl http://127.0.0.1:46564/v1/models

# auth enabled (CLAUDEAI_API_KEYS=key1,key2)
curl -H "Authorization: Bearer key1" http://127.0.0.1:46564/v1/models
```

## API

OpenAI clients use the base URL `http://<host>:<http-port>/v1` (public server). Same auth rules as above.

| Endpoint | Description |
|---|---|
| `GET /v1/models` | `{"object":"list","data":[{"id","object":"model","created","owned_by":"anthropic"}]}`. Ids come from `-claude-models`; `created` is the process start time |
| `GET /v1/models/{model}` | One advertised model, or 404 `model_not_found` |
| `POST /v1/chat/completions` | Non-streaming chat completion |

### Chat completions request fields

- **Used**: `model` (empty uses `-claude-model`; passed to the CLI as is, so aliases and full ids work), `messages` (required), `stream` (must be `false`/absent).
- **Messages**: roles `system` and `developer` are merged (in order, blank-line separated) into the system prompt; `user` and `assistant` form the conversation, which must end with a non-empty `user` message. `content` is a string or an array of `{"type":"text","text":...}` parts (joined with newlines).
- **Ignored without error** (not echoed): `temperature`, `top_p`, `max_tokens`, `max_completion_tokens`, `stop`, `presence_penalty`, `frequency_penalty`, `seed`, `user`, `stream_options`, `response_format`, `logit_bias`, `metadata`, and any unknown field.
- **Rejected with 400**: `tools`/`functions`, `tool_calls`/`function_call` and `tool`/`function` roles (`unsupported_parameter`; tool calling is not supported), `n` > 1 (`unsupported_parameter`), `stream: true` (`streaming_not_supported`), content parts other than text (`unsupported_content_type`).
- Body limit 8 MiB (`413 request_too_large`).

Response: `chat.completion` with one choice. `finish_reason` is `length` when Claude stopped on `max_tokens`, otherwise `stop`. `usage.prompt_tokens` = input + cache creation + cache read tokens.

### Errors

All errors use the OpenAI body `{"error":{"message","type","param":null,"code"}}`. Upstream messages are fixed and sanitized; CLI output is only logged.

| Status | `code` | Cause |
|---|---|---|
| 400 | `invalid_json`, `invalid_request_body`, `invalid_messages`, `invalid_request`, `unsupported_parameter`, `unsupported_content_type`, `streaming_not_supported` | Bad request (see above) |
| 401 | `missing_api_key`, `invalid_api_key` | Auth enabled and key missing/wrong |
| 404 | `model_not_found` | Model unknown to the CLI |
| 413 | `request_too_large` | Body over 8 MiB |
| 429 | `rate_limit_exceeded` | Claude usage limit reached |
| 429 | `server_busy` | All `-claude-max-concurrency` slots busy (`Retry-After: 1`) |
| 502 | `upstream_auth_error` | The Claude login is broken (your API key was fine) |
| 502/500 | `upstream_error` | CLI failure |
| 504 | `upstream_timeout` | Exceeded `-claude-timeout` |

If the client disconnects mid-request no body is written (logged at debug, recorded as 499).

```bash
# auth disabled
curl -s http://127.0.0.1:46564/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"haiku","messages":[{"role":"user","content":"Reply with the single word OK"}]}'

# auth enabled (CLAUDEAI_API_KEYS=key1)
curl -s http://127.0.0.1:46564/v1/chat/completions -H 'Authorization: Bearer key1' \
  -H 'Content-Type: application/json' \
  -d '{"model":"sonnet","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"Hi"}]}'
```

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:46564/v1", api_key="anything")  # use a real key when auth is enabled
r = client.chat.completions.create(model="haiku", messages=[{"role": "user", "content": "Hi"}])
print(r.choices[0].message.content)
```

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
