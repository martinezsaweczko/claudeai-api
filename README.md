# claudeai-api

[![CI](https://github.com/martinezsaweczko/claudeai-api/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/martinezsaweczko/claudeai-api/actions/workflows/ci.yml)
[![Coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/martinezsaweczko/claudeai-api/badges/coverage.json)](https://github.com/martinezsaweczko/claudeai-api/actions/workflows/ci.yml)
[![Release](https://github.com/martinezsaweczko/claudeai-api/actions/workflows/release.yml/badge.svg)](https://github.com/martinezsaweczko/claudeai-api/actions/workflows/release.yml)
[![Latest release](https://img.shields.io/github/v/release/martinezsaweczko/claudeai-api)](https://github.com/martinezsaweczko/claudeai-api/releases/latest)

OpenAI-compatible HTTP API (`/v1/models`, `/v1/chat/completions`, including SSE streaming) backed by the Claude Code CLI. The service is stateless: the client sends the full conversation on every request and each request runs a fresh `claude` process. Personal use only.

> Status: `GET /v1/models` and `POST /v1/chat/completions` (including SSE streaming with `stream: true`) are implemented.

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
| `-skills-repo` | empty | Git URL of a skills repository (ssh `git@host:o/r.git`, https, `file://` or a local path; URLs with embedded credentials are rejected). Empty disables skills |
| `-skills-ref` | `main` | Branch, tag or full commit sha to serve |
| `-skills-dir` | `<user cache dir>/claudeai-api/skills` | State directory (bare clone cache and read-only `versions/<sha>/`) |
| `-skills-refresh` | `5m` | Re-sync interval; `0` syncs once at startup; otherwise at least `30s` |
| `-skills-ssh-key` | empty | Path to a read-only deploy key file for ssh URLs (only the path is configured) |
| `-skills-known-hosts` | empty | Path to a `known_hosts` file; host keys are always verified |
| `-skills-sync-timeout` | `2m` | Max wall time of one sync |

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
| `POST /v1/chat/completions` | Chat completion, JSON or SSE (`stream: true`) |

Internal server (never expose it publicly): `GET /api/v1/skills`, `POST /api/v1/skills/refresh` (see [Skills](#skills)).

### Chat completions request fields

- **Used**: `model` (empty uses `-claude-model`; passed to the CLI as is, so aliases and full ids work), `messages` (required), `stream` (SSE when `true`), `stream_options.include_usage` (only with `stream: true`).
- **Messages**: roles `system` and `developer` are merged (in order, blank-line separated) into the system prompt; `user` and `assistant` form the conversation, which must end with a non-empty `user` message. `content` is a string or an array of `{"type":"text","text":...}` parts (joined with newlines).
- **Ignored without error** (not echoed): `temperature`, `top_p`, `max_tokens`, `max_completion_tokens`, `stop`, `presence_penalty`, `frequency_penalty`, `seed`, `user`, `response_format`, `logit_bias`, `metadata`, and any unknown field.
- **Rejected with 400**: `tools`/`functions`, `tool_calls`/`function_call` and `tool`/`function` roles (`unsupported_parameter`; tool calling is not supported), `n` > 1 (`unsupported_parameter`), content parts other than text (`unsupported_content_type`).
- Body limit 8 MiB (`413 request_too_large`).

- Trailing data after the JSON body is rejected (`invalid_json`); whitespace is fine.

Response: `chat.completion` with one choice. `finish_reason` is `length` when Claude stopped on `max_tokens`, otherwise `stop`. `usage.prompt_tokens` = input + cache creation + cache read tokens.

### Errors

All errors use the OpenAI body `{"error":{"message","type","param":null,"code"}}`. Upstream messages are fixed and sanitized; CLI output is only logged.

| Status | `code` | Cause |
|---|---|---|
| 400 | `invalid_json`, `invalid_request_body`, `invalid_messages`, `invalid_request`, `unsupported_parameter`, `unsupported_content_type` | Bad request (see above) |
| 401 | `missing_api_key`, `invalid_api_key` | Auth enabled and key missing/wrong |
| 404 | `model_not_found` | Model unknown to the CLI |
| 413 | `request_too_large` | Body over 8 MiB |
| 429 | `rate_limit_exceeded` | Claude usage limit reached |
| 429 | `server_busy` | All `-claude-max-concurrency` slots busy (`Retry-After: 1`) |
| 502 | `upstream_auth_error` | The Claude login is broken (your API key was fine) |
| 502/500 | `upstream_error` | CLI failure |
| 504 | `upstream_timeout` | Exceeded `-claude-timeout` |

If the client disconnects mid-request no body is written (logged at debug, recorded as 499).

### Streaming (SSE)

With `"stream": true` the response is `text/event-stream` (`Cache-Control: no-cache`, `X-Accel-Buffering: no`). Each event is `data: <json>\n\n`, flushed immediately, and the stream ends with `data: [DONE]`:

1. a `chat.completion.chunk` with `delta: {"role":"assistant","content":""}`;
2. one chunk per text delta, `delta: {"content":"..."}`, `finish_reason: null`;
3. a chunk with `delta: {}` and `finish_reason` `stop` (or `length` when Claude stopped on `max_tokens`);
4. only when `stream_options.include_usage` is `true`: a chunk with `"choices":[]` and `usage` (`prompt_tokens` = input + cache creation + cache read, `completion_tokens`, `total_tokens`). Without it no `usage` is sent.

All chunks share `id`, `created` and `model`. The model is the requested one (or `-claude-model`): the first chunk goes out before the CLI reports which model it resolved.

**Errors.** The response is committed on the first delta. A failure before that (auth, unknown model, rate limit, busy, timeout, a failed run with no output) is the normal JSON error with the same status as the non-streaming call. A failure after that is delivered as one event `data: {"error":{"message","type","param":null,"code"}}` (same sanitized body) followed by `data: [DONE]`; the HTTP status stays 200. If the client disconnects the CLI process is killed and nothing more is written.

**Proxies.** There is no heartbeat, so a reverse proxy that buffers or has a short idle timeout can break long silent gaps: disable buffering (`X-Accel-Buffering: no` is sent for nginx) and raise `proxy_read_timeout`. The server clears its write deadline for the stream.

```bash
curl -N http://127.0.0.1:46564/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"haiku","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"Count from 1 to 5"}]}'
```

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:46564/v1", api_key="unused-unless-keys-are-set")
stream = client.chat.completions.create(
    model="haiku",
    messages=[{"role": "user", "content": "Count from 1 to 5"}],
    stream=True,
)
for chunk in stream:
    if chunk.choices and chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="", flush=True)
```

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

## Skills

Optional. Skills (Claude Code `SKILL.md` folders) live in a **separate private git repository** laid out as a plugin marketplace (`.claude-plugin/marketplace.json` -> `plugins/<plugin>/skills/<name>/SKILL.md`). The gateway clones it with the `git` binary, validates it and serves the skills to every `claude -p` run. Skills stay **disabled** unless `-skills-repo` is set; with skills disabled, with no successful sync yet, or with a repository that has zero valid skills, requests run exactly as before.

### How it works

1. A background syncer fetches `-skills-ref` into a bare cache, validates the tree (no symlinks, size and count limits, `name` in the frontmatter must equal the directory) and copies **only** the validated skills into a read-only `-skills-dir/versions/<sha>/.claude/skills/<name>/`. Hooks, MCP servers, agents and commands from the repository are never copied.
2. Each request resolves the current snapshot once, runs the CLI with that directory as its working directory and the flags `--tools Skill --setting-sources project` (and without `--disable-slash-commands`, which also disables skills). A sync that finishes mid-run does not affect running requests.
3. The system prompt gets one extra sentence telling the model it has skills through the Skill tool and should use one whenever its description matches the request. The model still decides: write clear skill `description`s, since that is what it matches against. Streaming and non-streaming responses contain the same text: only assistant text is forwarded (tool calls and tool results never are), and when the model writes text in several turns (for example a sentence before calling a skill), the turns are joined with a blank line.

`--plugin-dir` is deliberately **not** used: a plugin SessionStart hook executed even with the isolation flags, so a compromised skills repository would get code execution. Skills-only materialization cannot carry hooks.

### Cost

Enabling skills raises the prompt from about 0.9k to about 7.8k tokens per request, mostly Claude Code's bundled skills and the Skill tool definition, and mostly served from the prompt cache. A request that actually invokes a skill takes at least one extra model turn: measured with haiku, `usage.prompt_tokens` was about 10.2k (it sums the input of every turn) and latency about 3s longer than without a skill.

### Setup with a read-only deploy key

```bash
mkdir -p ~/.config/claudeai-api && cd ~/.config/claudeai-api
ssh-keygen -t ed25519 -N "" -f skills_deploy_key -C claudeai-api-skills
gh repo deploy-key add skills_deploy_key.pub -R OWNER/skills-repo -t claudeai-api-skills   # read-only unless --allow-write
gh api meta --jq '.ssh_keys[] | "github.com " + .' > known_hosts   # pinned GitHub host keys
chmod 600 skills_deploy_key
```

```bash
./build/main -http-address 127.0.0.1 \
  -skills-repo git@github.com:OWNER/skills-repo.git \
  -skills-ssh-key ~/.config/claudeai-api/skills_deploy_key \
  -skills-known-hosts ~/.config/claudeai-api/known_hosts \
  -skills-dir /var/lib/claudeai-api/skills
```

Only the key **path** is configured and logged; the key content is never read by the gateway, logged or returned. Host keys are always verified (`StrictHostKeyChecking=yes`). A missing key or known_hosts file stops startup with an error; a transient git failure only logs a warning and the last good snapshot stays in use.

### Refresh and status (internal server)

The syncer re-syncs every `-skills-refresh` (default 5m, jittered, with backoff after failures). The startup does one bounded initial sync (at most 20s) before the servers accept traffic.

```bash
curl http://127.0.0.1:9000/api/v1/skills                    # enabled, commit, skills, skipped, sync health
curl -X POST http://127.0.0.1:9000/api/v1/skills/refresh   # 202 {"status":"triggered"}; 404 skills_disabled when off
```

`POST .../refresh` is non-blocking and coalesced; poll `GET .../skills` for the new commit (for example from a repository webhook relay). Both endpoints are **unauthenticated, like health and metrics**: the internal port must never be exposed publicly. When skills are disabled `GET` answers `{"enabled":false}`.

### Threat model

- The deploy key is read-only and scoped to the one repository; a leaked key allows reading that repository only.
- The skills repository is trusted input: its skill text is given to the model, so a compromised repository can inject prompts (prompt injection). The model only has the Skill tool (no shell, no files, no network tools), which bounds what an injected prompt can do, but it can shape answers. Review skill changes like code.
- The CLI reads project settings from its working directory **and its ancestors**: keep `-skills-dir` in a dedicated directory with no `CLAUDE.md` or `.claude/settings*` above it.
- The syncer never executes repository content (bare clone, `ls-tree`/`cat-file`, no checkout, hooks or symlinks).
- Container note: the image needs `git` and `openssh-client` for ssh remotes (the Containerfile is handled in T8).

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

- **Traces**: OTLP gRPC (`-o11-tracer-endpoint tempo:4317`), see [Traces](#traces).
- **Logs**: slog logs with `trace_id` / `span_id` injected from the request or run context (also `Skills refresh requested`, sync and run logs). Prompts, message content and key paths are never logged.
- **Metrics**: Prometheus on the internal server (`-o11-prometheus-path`, default `/metrics`), see below.

### Metrics

Template metrics: `http_server_requests_total`, `http_server_request_duration_seconds`, `http_server_requests_in_flight` (RED for every route, labelled by method/route/status), `repo_queries_total`, `repo_query_duration_seconds`, `external_api_{calls_total,duration_seconds}`.

Claude and skills metrics (meters `claudeai-api/claude` and `claudeai-api/skills`). The names below are exactly what Prometheus exposes (histograms add `_bucket`, `_sum`, `_count`); the exporter also adds `otel_scope_name`/`otel_scope_version`/`otel_scope_schema_url` labels, which queries can ignore.

| Name | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `claude_runs_total` | counter | `model`, `outcome` | Finished Claude CLI runs. `outcome`: `success`, `auth`, `rate_limited`, `timeout`, `busy` (no concurrency slot before the request ended), `cli_error`, `not_found` (binary missing), `invalid_model`, `invalid_request`, `canceled` (client went away while running), `other`. |
| `claude_run_duration_seconds` | histogram | `model` | Whole call, including the wait for a concurrency slot. Buckets 0.5 .. 300 s. |
| `claude_time_to_first_token_seconds` | histogram | `model` | From the start of the call (so it includes the queue wait) to the first text delta. Only runs that produced text. With skills, a tool-call turn comes first, so expect seconds more. Buckets 0.25 .. 60 s. |
| `claude_queue_wait_seconds` | histogram | `model` | Time waiting for a concurrency slot (`-claude-max-concurrency`); runs that found a free slot record 0. Buckets 0.01 .. 60 s. |
| `claude_runs_in_flight` | gauge | none | CLI processes running now (queued callers are not counted). Read at scrape time. |
| `claude_tokens_total` | counter | `model`, `type` | Tokens from the CLI result. `type`: `input` (uncached input only), `output`, `cache_creation`, `cache_read`. Prompt side = `input + cache_creation + cache_read`. The CLI sums every turn of the run, so a run that uses a skill (several turns) counts more than the final prompt. Failed runs that returned usage are counted too. |
| `claude_cost_usd_total` | counter | `model` | Sum of the CLI `total_cost_usd`: list-price **estimate**, not billing (a Max subscription is not billed per token). |
| `claude_skill_invocations_total` | counter | `skill` | Skill tool uses seen in the stream. Names not in the currently loaded snapshot count as `other`. |
| `skills_sync_total` | counter | `outcome` | Skills repo syncs: `success` (new version materialized), `noop` (commit already current), `error`. |
| `skills_sync_duration_seconds` | histogram | none | Duration of a sync. Buckets 0.1 .. 60 s. |
| `skills_loaded` | gauge | none | Skills in the current snapshot (0 without one). Read at scrape time from the syncer. |
| `skills_skipped` | gauge | none | Skills ignored by validation in the current snapshot. |
| `skills_sync_consecutive_failures` | gauge | none | Failed syncs in a row (0 after a success). |
| `skills_last_success_timestamp_seconds` | gauge | none | Unix time of the last successful sync; absent until one succeeds. Use `time() - skills_last_success_timestamp_seconds` for staleness. |
| `skills_info` | gauge | `commit`, `ref` | Always 1, for the CURRENT snapshot only (12-char commit), so replaced commits do not accumulate series. Absent without a snapshot. |

The `skills_*` instruments exist only when `-skills-repo` is set. There is no `chat_completions_total`: route and status come from `http_server_requests_total`, and `claude_runs_total` already carries the outcome per model, so a third counter would add no information.

**Cardinality rule.** A label value may only come from a bounded set. Client-controlled strings (the `model` field of a request, skill names the model emits) must never create series: `model` is the model id the CLI reports when it ran (a bounded set of real ids), otherwise the requested model only if it is in `-claude-models` or is the default model, else `other`; `skill` is a loaded skill name or `other`; `outcome` and `type` are fixed enums. New labels must follow the same rule (normalize in `app/metrics.go`, test the normalizer). Observers are cheap, non-blocking and panic-safe; a metrics bug cannot fail a request or a sync.

A Grafana dashboard and example Prometheus alert rules for these metrics are in [`grafana/`](grafana/README.md). `go test ./grafana/` fails if the dashboard or the alerts reference a metric or label that the app does not expose.

### Traces

One request is one trace: the `otelhttp` server span (`POST /v1/chat/completions`) is the parent of `claude.run` (same trace id), streaming or not. Skill syncs are their own traces (`skills.sync`, started by the sync loop or by `POST /api/v1/skills/refresh`).

`claude.run` attributes: `claude.model` (requested, or the default), `claude.model.resolved`, `claude.outcome`, `claude.exit_code`, `claude.duration_ms`, `claude.queue_wait_ms`, `claude.ttft_ms` (only when text was produced), `claude.num_turns`, `claude.tokens.{input,output,cache_creation,cache_read}`, `claude.cost_usd`, `claude.skills.active` (bool, always set), `claude.skills.commit` (12-char commit of the snapshot used, when skills are active) and `claude.skills.invoked` (string array, only when the model used skills). Event `first_token` (attribute `claude.elapsed_ms`) marks the first text delta. Errors are recorded with `RecordError` and an error status. `skills.sync` attributes: `skills.repo`, `skills.ref`, `skills.commit`, `skills.outcome`, `skills.count`, `skills.skipped`. Attributes carry no prompt text. Span attributes are not limited to bounded values (unlike metric labels), so `claude.model` holds whatever the client sent.

### Forwarding the Claude Code CLI's own telemetry (optional)

The gateway builds each child's environment from its own (`os.Environ`, minus `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN`, see `filterEnv` in `services/claude/render.go`), so variables set on the gateway process reach `claude -p` with no code change. For example `CLAUDE_CODE_ENABLE_TELEMETRY=1` with `OTEL_METRICS_EXPORTER=otlp`, `OTEL_LOGS_EXPORTER=otlp` and `OTEL_EXPORTER_OTLP_ENDPOINT=...` makes every run export the CLI's metrics and log events. Traces from the CLI are beta (`CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1`). Prompt logging stays off unless you enable it explicitly (`OTEL_LOG_USER_PROMPTS=1`); do not enable it here, since prompts would leave the gateway. This duplicates part of the data above (tokens, cost, duration) from a second source and adds one export per short-lived process; check the current CLI documentation for the exact variable names before relying on them.

## License

See [LICENSE](LICENSE).
