# claudeai-api

[![CI](https://github.com/martinezsaweczko/claudeai-api/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/martinezsaweczko/claudeai-api/actions/workflows/ci.yml)
[![Coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/martinezsaweczko/claudeai-api/badges/coverage.json)](https://github.com/martinezsaweczko/claudeai-api/actions/workflows/ci.yml)
[![Release](https://github.com/martinezsaweczko/claudeai-api/actions/workflows/release.yml/badge.svg)](https://github.com/martinezsaweczko/claudeai-api/actions/workflows/release.yml)
[![Latest release](https://img.shields.io/github/v/release/martinezsaweczko/claudeai-api)](https://github.com/martinezsaweczko/claudeai-api/releases/latest)

An OpenAI-compatible HTTP API (`/v1/models`, `/v1/chat/completions`, including SSE streaming) that answers with the **Claude Code CLI**, authenticated with your own Claude Max subscription. Point any OpenAI client (Open WebUI, SDKs, `curl`) at it and get Claude.

The service is **stateless**: the client sends the whole conversation on every request and each request runs a fresh `claude -p` process. Any container can serve any request, so you can run several behind a load balancer.

> **Personal use only.** This gateway is meant for you, with your own subscription. Anthropic's terms do not allow routing Max credentials on behalf of other users, and your usage limits are shared across every container or client that uses the same login (see [Legal and compliance](https://code.claude.com/docs/en/legal-and-compliance)). The gateway limits concurrency (`-claude-max-concurrency`) for that reason; do not offer it to third parties.

## Contents

- [Architecture](#architecture)
- [Quickstart A: run locally](#quickstart-a-run-locally)
- [Quickstart B: run in a container](#quickstart-b-run-in-a-container)
- [Authentication](#authentication)
- [Configuration reference](#configuration-reference)
- [API](#api)
- [Skills](#skills)
- [Web access](#web-access)
- [Observability](#observability)
- [Operations](#operations)
- [Troubleshooting](#troubleshooting)
- [Limits and non-goals](#limits-and-non-goals)
- [Security notes](#security-notes)
- [Development](#development)

## Architecture

```
                         +----------------------- gateway (one container, ONE port :46564) -----------------------+
 OpenAI client           |  /v1/models  /v1/chat/completions   --> concurrency limit --> claude -p (per request)|--> Anthropic
 (Open WebUI, SDK, curl) |        |                                  (own process group, isolation flags)      |    (Max login via
        +--------------> |  tracing -> metrics -> logging -> API-key auth                                        |     CLAUDE_CODE_OAUTH_TOKEN)
        Bearer key       |                                                                                      |
 Prometheus / Grafana    |  /metrics  /api/v1/skills[/refresh]       (same API-key auth when keys are set)       |
        +--------------> |  /api/v1/health  /api/v1/version  /api/v1/docs/   (always public)                    |
        Bearer key       |                                                                                      |
 private skills repo     |  skills syncer: git (ssh, read-only deploy key) --> validated, read-only snapshot     |
        <--------------- |  used as the cwd of every claude run (--tools Skill --setting-sources project)        |
                         +--------------------------------------------------------------------------------------+
```

Package-by-layer with consumer-defined interfaces and manual DI (`app/`):

```
cmd/main.go        bootstrap
app/               App builder + wiring
config/            CLI flag configuration with validation
http_server/       net/http server wrapper (synchronous bind, graceful stop)
http/              handlers (chat, models, health, version, skills, metrics, swagger) + middleware
services/claude    Claude CLI backend (stream-json parsing, errors, process control)
services/skills    git-backed skills syncer
services/          shared service helpers (instrumented HTTP client)
o11/               OTel setup, metrics factories, trace-aware slog handler
grafana/           dashboard + example alert rules
```

One HTTP server, one port (`-http-port`, default 46564), so the Swagger UI and the API share an origin and "Try it out" works. The write timeout (`-http-write-timeout`) defaults to 0 (disabled) so long SSE streams are not cut. It listens on **127.0.0.1** by default (secure by default for a personal tool); the container image passes `0.0.0.0` explicitly so the published port works.

| Route | Description | Auth |
|---|---|---|
| `GET /v1/models`, `GET /v1/models/{model}`, `POST /v1/chat/completions` | OpenAI-compatible API | protected |
| `GET /metrics` (`-o11-prometheus-path`) | Prometheus metrics | protected |
| `GET /api/v1/skills`, `POST /api/v1/skills/refresh` | Skills status and refresh | protected |
| `GET /api/v1/health`, `GET /api/v1/version` | Probes and build info | public |
| `GET /api/v1/docs/` (Swagger UI, `doc.json`), `GET /docs` (redirect to the UI) | Swagger for the whole service | public |

"Protected" means `Authorization: Bearer <key>` is required **when `CLAUDEAI_API_KEYS` is set**; with no keys configured everything is open. "Public" never needs a key (so container health checks and the Swagger UI keep working).

> **Breaking change.** The second "internal" server is gone: the flags `-internal-address` and `-internal-port` were removed (passing them now aborts startup with `flag provided but not defined`), and `-health-check` now defaults to port 46564 instead of 9000. Health, version, metrics, Swagger and the skills endpoints moved onto the API port; point Prometheus, probes and bookmarks at `46564`.

## Quickstart A: run locally

1. Install the Claude Code CLI ([setup docs](https://code.claude.com/docs/en/setup)) and log in once, or create a long-lived token:

   ```bash
   curl -fsSL https://claude.ai/install.sh | bash
   claude setup-token            # prints a token valid for one year; copy it
   export CLAUDE_CODE_OAUTH_TOKEN=<token>   # optional if `claude` is already logged in on this machine
   ```

2. Run the gateway (loopback only, no API keys):

   ```bash
   make run                      # go run ./cmd  (127.0.0.1:46564)
   # or: go build -o build/main ./cmd && ./build/main
   ```

3. Call it:

   ```bash
   curl -s http://127.0.0.1:46564/v1/models
   curl -s http://127.0.0.1:46564/v1/chat/completions -H 'Content-Type: application/json' \
     -d '{"model":"haiku","messages":[{"role":"user","content":"Reply with the single word OK"}]}'
   ```

   ```python
   from openai import OpenAI

   client = OpenAI(base_url="http://127.0.0.1:46564/v1", api_key="anything")  # use a real key when auth is enabled
   r = client.chat.completions.create(model="haiku", messages=[{"role": "user", "content": "Hi"}])
   print(r.choices[0].message.content)
   ```

   Open WebUI and similar tools: add an OpenAI-compatible connection with base URL `http://127.0.0.1:46564/v1` (from another container use the host or service name) and the API key if you set `CLAUDEAI_API_KEYS` (any non-empty string otherwise).

## Quickstart B: run in a container

The image (`Containerfile`) contains the gateway, `git`, `openssh-client` and the Claude Code CLI **pinned to 2.1.309** (`--build-arg CLAUDE_VERSION=...` to change it). It runs as `appuser` (uid/gid 1001, `HOME=/home/appuser`), keeps skills state in `/var/lib/claudeai-api`, and contains no credentials.

```bash
make container            # podman build --format docker ... -> claudeai-api:latest
# docker: docker build -f Containerfile -t claudeai-api:latest .
```

Run it (rootless podman, verified). Put the token in an env file with mode 600 instead of the command line:

```bash
printf 'CLAUDE_CODE_OAUTH_TOKEN=%s\n' '<token from claude setup-token>' > .env && chmod 600 .env   # .env is git-ignored

podman run -d --name claudeai-api \
  --userns=keep-id:uid=1001,gid=1001 \
  --read-only --tmpfs /tmp:rw,mode=1777 --tmpfs /home/appuser:rw,mode=1777 \
  -v claudeai-state:/var/lib/claudeai-api \
  -p 127.0.0.1:46564:46564 \
  --env-file .env \
  claudeai-api:latest
```

With skills (read-only deploy key and pinned `known_hosts`, see [Skills](#skills)):

```bash
S=~/.config/claudeai-api
podman run -d --name claudeai-api \
  --userns=keep-id:uid=1001,gid=1001 \
  --read-only --tmpfs /tmp:rw,mode=1777 --tmpfs /home/appuser:rw,mode=1777 \
  -v claudeai-state:/var/lib/claudeai-api \
  -v $S/skills_deploy_key:/run/secrets/skills_deploy_key:ro \
  -v $S/known_hosts:/run/secrets/known_hosts:ro \
  -p 127.0.0.1:46564:46564 --env-file .env \
  claudeai-api:latest \
  -http-address 0.0.0.0 -skills-dir /var/lib/claudeai-api/skills \
  -skills-repo git@github.com:OWNER/skills-repo.git \
  -skills-ssh-key /run/secrets/skills_deploy_key -skills-known-hosts /run/secrets/known_hosts
```

(Arguments after the image name replace the image's default `CMD`, which is why the bind and `-skills-dir` flags are repeated.) `podman ps` shows the health state; `podman logs claudeai-api` shows the sync result.

Notes:

- **Why `--userns=keep-id:uid=1001,gid=1001`.** ssh refuses a private key that is not owned by the user running it. With this option your host user is uid 1001 inside the container, so a key you own (mode 600) is accepted as is, and the `:ro` mount leaves the original untouched. Verified with rootless podman 5.4. The alternative `:U` volume option chowns the host file, so use it only on a copy.
- **docker.** There is no `keep-id`: copy the key and `known_hosts` to a directory you control, `chown 1001:1001` them, `chmod 0400` the key, mount them `:ro`, and drop `--userns`. Docker has not been exercised for this project (only podman was), so treat these steps as unverified.
- **`--read-only`.** The root filesystem is read-only; `/tmp`, `/home/appuser` (the CLI writes its config there) and the state volume are the only writable places. podman creates tmpfs mounts root-owned and has no `uid=` option, hence `mode=1777`; the mounts are private to the container. Drop `--read-only` and the tmpfs flags if you prefer.
- **Ports.** One port (46564), published only on `127.0.0.1`. The container binds `0.0.0.0` and may run without API keys, so publishing it on all interfaces (`-p 46564:46564`) without `CLAUDEAI_API_KEYS` exposes your subscription, `/metrics` and the skills refresh endpoint; the startup warning says so. Other containers on the same network reach it as `claudeai-api:46564` (send the Bearer key when keys are set).
- **Health check.** The image runs `claudeai-api -health-check` (port 46564) every 30s (health is public and answers on the single port, no key needed). `podman build` must use `--format docker` to keep the `HEALTHCHECK` instruction (`make container` does).
- **State volume.** `/var/lib/claudeai-api` is a disposable cache (bare clone and materialized skills). It can be a named volume or a tmpfs; losing it only costs one re-clone.
- **Claude CLI in the image.** Installed from the official release binary (no Node.js), checked against Anthropic's signed manifest: the release signing key must match the pinned fingerprint `31DD DE24 DDFA B679 F42D 7BD2 BAA9 29FF 1A7E CACE`, the detached signature of `manifest.json` must verify against it, and the binary's SHA-256 must match the manifest. Not verified: Linux binaries carry no per-binary code signature. The image sets `DISABLE_UPDATES=1` (the version stays pinned) and `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` (no telemetry or error reporting), both documented in the Claude Code docs.
- **Compose.** [`compose.example.yaml`](compose.example.yaml) has the same setup for `podman compose` / `docker compose`, including the scaling notes.

## Authentication

Two separate layers:

1. **Gateway to Claude: `CLAUDE_CODE_OAUTH_TOKEN`.** Run `claude setup-token` once on a machine where you are logged in with your Max subscription; it prints a token that is valid for **one year**. Pass it to every container as an environment variable. To rotate it, run `claude setup-token` again, update the secret and restart the containers (a running process keeps using the old token). When it expires or is revoked requests fail with `502 upstream_auth_error`. If the variable is unset the CLI falls back to its own stored login (convenient locally; there is none in a container).
2. **Client to gateway: `CLAUDEAI_API_KEYS` (optional).** Comma-separated keys; when set, the protected routes (`/v1/*`, `/metrics`, `/api/v1/skills`, `/api/v1/skills/refresh`) require `Authorization: Bearer <key>` and answer failures with `401`, `WWW-Authenticate: Bearer` and an OpenAI-style body (`missing_api_key` / `invalid_api_key`). Health, version and the Swagger UI stay public. When unset or empty, authentication is **off** for everything.

Secrets are read from environment variables, not flags, because flags are visible in process listings (`ps`, `/proc`). They are never logged: the startup log only says whether they are set and how many keys there are. `ANTHROPIC_API_KEY` must not be set: it overrides the subscription login and would bill the API key; the gateway warns if it is set and strips it (and `ANTHROPIC_AUTH_TOKEN`) from the Claude child processes.

```bash
curl http://127.0.0.1:46564/v1/models                                      # auth disabled
curl -H "Authorization: Bearer key1" http://127.0.0.1:46564/v1/models      # CLAUDEAI_API_KEYS=key1,key2
```

## Configuration reference

All configuration is flags, except secrets (environment variables). `./build/main -h` prints the same list.

| Flag | Default | Description |
|---|---|---|
| `-http-address` | `127.0.0.1` | Bind address. `0.0.0.0` listens on every interface (the container image sets it) |
| `-http-port` | `46564` | Port of the single server (API, health, metrics, Swagger, skills) |
| `-http-timeout` | `30` | Idle timeout in seconds |
| `-http-read-timeout` | `10` | Read timeout in seconds |
| `-http-write-timeout` | `0` | Write timeout in seconds; `0` disables it, needed for SSE |
| `-log-level` | `info` | `debug`, `info`, `warn`, `error` |
| `-log-file-path` | `stdout` | Log destination |
| `-o11-tracer-endpoint` | empty | OTLP gRPC endpoint without scheme (for example `tempo:4317`) |
| `-o11-prometheus-path` | `/metrics` | Metrics path (protected when API keys are set) |
| `-o11-environment` | `dev` | Environment tag in telemetry |
| `-claude-bin` | `claude` | Claude Code CLI binary name or path |
| `-claude-model` | `sonnet` | Default model alias when a request names none |
| `-claude-models` | `sonnet,opus,haiku` | Ids advertised by `GET /v1/models` (no empty entries; duplicates removed; the default model is added if missing). Any model the CLI accepts still works in requests |
| `-claude-timeout` | `5m` | Max wall time per request |
| `-claude-max-concurrency` | `4` | Max simultaneous CLI processes in this process (>= 1) |
| `-claude-web` | `search` | Web tools for the model: `none` (no web access), `search` (WebSearch, executed by Anthropic) or `full` (WebSearch + WebFetch; WebFetch fetches from the gateway host, see [Web access](#web-access)) |
| `-skills-repo` | empty | Git URL of the skills repository (ssh `git@host:o/r.git`, https, `file://` or a local path; URLs with embedded credentials are rejected). Empty disables skills |
| `-skills-ref` | `main` | Branch, tag or full commit sha to serve |
| `-skills-dir` | `<user cache dir>/claudeai-api/skills` | State directory (bare clone cache and read-only `versions/<sha>/`). The image uses `/var/lib/claudeai-api/skills` |
| `-skills-refresh` | `5m` | Re-sync interval; `0` syncs once at startup; otherwise at least `30s` |
| `-skills-ssh-key` | empty | Path to the read-only deploy key for ssh URLs (only the path is configured) |
| `-skills-known-hosts` | empty | Path to a `known_hosts` file; host keys are always verified |
| `-skills-sync-timeout` | `2m` | Max wall time of one sync |

| Environment variable | Description |
|---|---|
| `CLAUDE_CODE_OAUTH_TOKEN` | Token from `claude setup-token` (one year). Optional locally, required in a container. Never logged |
| `CLAUDEAI_API_KEYS` | Optional comma-separated client API keys. Unset or empty means authentication is off |
| `ANTHROPIC_API_KEY` | Do not set (see [Authentication](#authentication)) |

The child `claude` processes inherit the gateway environment (minus the two variables above), so Claude Code's own settings variables, for example `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, apply to them too.

## API

OpenAI clients use the base URL `http://<host>:<http-port>/v1` (same server and port as everything else). Same auth rules as above. Swagger UI for the whole service is at `/api/v1/docs/` (also `/docs`), on the same origin as the API, so "Try it out" works; click **Authorize** and paste the key when `CLAUDEAI_API_KEYS` is set (generated by [swaggo/swag](https://github.com/swaggo/swag), `make swagger`).

| Endpoint | Description |
|---|---|
| `GET /v1/models` | `{"object":"list","data":[{"id","object":"model","created","owned_by":"anthropic"}]}`. Ids come from `-claude-models`; `created` is the process start time |
| `GET /v1/models/{model}` | One advertised model, or 404 `model_not_found` |
| `POST /v1/chat/completions` | Chat completion, JSON or SSE (`stream: true`) |

### Chat completions request fields

| Field | Support |
|---|---|
| `model` | Used. Empty uses `-claude-model`; passed to the CLI as is, so aliases and full ids work |
| `messages` | Used, required. Roles `system` and `developer` are merged (in order, blank-line separated) into the system prompt; `user` and `assistant` form the conversation, which must end with a non-empty `user` message. `content` is a string or an array of `{"type":"text","text":...}` parts (joined with newlines) |
| `stream` | Used: SSE when `true` |
| `stream_options.include_usage` | Used, only with `stream: true` |
| `temperature`, `top_p`, `max_tokens`, `max_completion_tokens`, `stop`, `presence_penalty`, `frequency_penalty`, `seed`, `user`, `response_format`, `logit_bias`, `metadata`, any unknown field | Ignored without error (not echoed) |
| `tools`, `functions`, `tool_calls`, `function_call`, roles `tool` / `function` | Rejected: 400 `unsupported_parameter` |
| `n` > 1 | Rejected: 400 `unsupported_parameter` |
| Content parts other than text (images, audio) | Rejected: 400 `unsupported_content_type` |

Body limit 8 MiB (`413 request_too_large`). Trailing data after the JSON body is rejected (`invalid_json`); whitespace is fine.

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

## Skills

Optional. Skills (Claude Code `SKILL.md` folders) live in a **separate private git repository** laid out as a plugin marketplace (`.claude-plugin/marketplace.json` -> `plugins/<plugin>/skills/<name>/SKILL.md`). The gateway clones it with the `git` binary, validates it and serves the skills to every `claude -p` run. Skills stay **disabled** unless `-skills-repo` is set; with skills disabled, with no successful sync yet, or with a repository that has zero valid skills, requests run exactly as before.

### How it works

1. A background syncer fetches `-skills-ref` into a bare cache, validates the tree (no symlinks, size and count limits, `name` in the frontmatter must equal the directory) and copies **only** the validated skills into a read-only `-skills-dir/versions/<sha>/.claude/skills/<name>/`. Hooks, MCP servers, agents and commands from the repository are never copied.
2. Each request resolves the current snapshot once, runs the CLI with that directory as its working directory and the flags `--tools Skill --setting-sources project` (plus the web tools when `-claude-web` is not `none`) (and without `--disable-slash-commands`, which also disables skills). A sync that finishes mid-run does not affect running requests.
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
./build/main \
  -skills-repo git@github.com:OWNER/skills-repo.git \
  -skills-ssh-key ~/.config/claudeai-api/skills_deploy_key \
  -skills-known-hosts ~/.config/claudeai-api/known_hosts \
  -skills-dir /var/lib/claudeai-api/skills
```

In a container, mount both files read-only under `/run/secrets/` as shown in [Quickstart B](#quickstart-b-run-in-a-container). Only the key **path** is configured and logged; the key content is never read by the gateway, logged or returned. Host keys are always verified (`StrictHostKeyChecking=yes`). A missing key or known_hosts file stops startup with an error; a transient git failure only logs a warning and the last good snapshot stays in use.

### Refresh and status

The syncer re-syncs every `-skills-refresh` (default 5m, jittered, with backoff after failures). The startup does one bounded initial sync (at most 20s) before the servers accept traffic.

```bash
curl http://127.0.0.1:46564/api/v1/skills                    # enabled, commit, skills, skipped, sync health
curl -X POST http://127.0.0.1:46564/api/v1/skills/refresh   # 202 {"status":"triggered"}; 404 skills_disabled when off
# with CLAUDEAI_API_KEYS set, add: -H "Authorization: Bearer <key>"
```

`POST .../refresh` is non-blocking and coalesced; poll `GET .../skills` for the new commit (for example from a repository webhook relay). Both endpoints share the API port and are **protected like `/v1`**: they require the Bearer key when `CLAUDEAI_API_KEYS` is set, and are open (so keep the port on loopback) when it is not. When skills are disabled `GET` answers `{"enabled":false}`.

## Web access

Without web tools the model cannot see anything after its training data and answers "I have no internet access" to questions about recent events. `-claude-web` grants the capability:

| Value | Tools given to the CLI | Notes |
|---|---|---|
| `none` | no web tools | Exactly the behavior before this flag existed |
| `search` (default) | `WebSearch` | Anthropic runs the search server-side and returns result titles and URLs. Nothing is fetched from the gateway host |
| `full` | `WebSearch` + `WebFetch` | `WebFetch` downloads pages **from the gateway host** (see the SSRF note) |

How it is wired: the web tools are added to `--tools` (next to `Skill` when skills are active) **and** to `--allowedTools`. Both are required: verified with the real CLI, `--tools WebSearch` alone is denied in headless mode ("no approval surface") and the model answers that it could not search. The system prompt gets one extra sentence telling the model to search for recent events, news, scores, prices and anything after its cutoff, and to cite sources. All isolation flags are unchanged.

- **Cost and latency.** A search adds model turns and nested search calls: one real question ("how did the last FC Barcelona match end?", haiku) used 2 searches, 3 turns, about 25-28 s and USD 0.026-0.039 list-price estimate, against about USD 0.004 and a few seconds without web. With the skills prompt the request reported about 31k prompt tokens. Expect `claude_cost_usd_total`, `claude_tokens_total` and `claude_run_duration_seconds` to rise for questions that trigger a search; they do not change for questions that do not.
- **Data leaves via Anthropic search.** The query the model writes is sent to Anthropic's web search backend. Do not enable it where prompts must never leave the machine for search.
- **Availability.** WebSearch needs a plan, model and provider that support it (it is not exposed on Amazon Bedrock). If it is unavailable the model answers that the search failed; there is no gateway error.
- **`full` and SSRF.** `WebFetch` runs on the machine that runs the gateway. A prompt-injected or malicious request can make the model fetch an attacker-chosen URL from there. The CLI documents that it refuses `localhost` and any hostname without a dot; its handling of private, link-local or cloud-metadata IP addresses for `WebFetch` is **not documented** (the rule for private addresses is documented for another tool only), so treat internal addresses as reachable. This is why `full` is opt-in and logs a WARN at startup. Use it only on a host with no sensitive internal network, or behind egress filtering. The fetch also sends the domain to Anthropic for a safety check, and returned page text is summarized by a model, so it can carry injected instructions.
- **Skills only guide.** The `web-research` skill in the skills repository tells the model how to research (primary sources, dates, citations, no guessing). It cannot grant internet access: without `-claude-web search|full` the tool does not exist.

Sources: <https://code.claude.com/docs/en/tools-reference> (WebFetch and WebSearch tool behavior), <https://code.claude.com/docs/en/security>.

## Observability

- **Traces**: OTLP gRPC (`-o11-tracer-endpoint tempo:4317`), see [Traces](#traces).
- **Logs**: slog logs with `trace_id` / `span_id` injected from the request or run context (also `Skills refresh requested`, sync and run logs). Prompts, message content and key paths are never logged.
- **Metrics**: Prometheus on the single port (`-o11-prometheus-path`, default `/metrics`; requires the Bearer key when `CLAUDEAI_API_KEYS` is set), see below. Scrape example:

  ```yaml
  scrape_configs:
    - job_name: claudeai-api
      metrics_path: /metrics
      static_configs: [{targets: ['claudeai-api:46564']}]
      authorization: { type: Bearer, credentials: <one of CLAUDEAI_API_KEYS> }   # omit when no keys are set
  ```
- **Dashboard and alerts**: [`grafana/`](grafana/README.md) has a Grafana dashboard (Claude runs, tokens and cost, skills, HTTP, system) and example Prometheus alert rules (`grafana/alerts.example.yml`). `go test ./grafana/` fails if the dashboard or the alerts reference a metric or label that the app does not expose.

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
| `claude_web_requests_total` | counter | `type` | Web tool uses of a run (`search`, `fetch`), from the result's `modelUsage`/`usage.server_tool_use` and the tool_use blocks, whichever is largest (the real CLI reports searches in `modelUsage` while `usage.server_tool_use` stays 0). Zero while `-claude-web none`. |
| `skills_sync_total` | counter | `outcome` | Skills repo syncs: `success` (new version materialized), `noop` (commit already current), `error`. |
| `skills_sync_duration_seconds` | histogram | none | Duration of a sync. Buckets 0.1 .. 60 s. |
| `skills_loaded` | gauge | none | Skills in the current snapshot (0 without one). Read at scrape time from the syncer. |
| `skills_skipped` | gauge | none | Skills ignored by validation in the current snapshot. |
| `skills_sync_consecutive_failures` | gauge | none | Failed syncs in a row (0 after a success). |
| `skills_last_success_timestamp_seconds` | gauge | none | Unix time of the last successful sync; absent until one succeeds. Use `time() - skills_last_success_timestamp_seconds` for staleness. |
| `skills_info` | gauge | `commit`, `ref` | Always 1, for the CURRENT snapshot only (12-char commit), so replaced commits do not accumulate series. Absent without a snapshot. |

The `skills_*` instruments exist only when `-skills-repo` is set. There is no `chat_completions_total`: route and status come from `http_server_requests_total`, and `claude_runs_total` already carries the outcome per model, so a third counter would add no information.

**Cardinality rule.** A label value may only come from a bounded set. Client-controlled strings (the `model` field of a request, skill names the model emits) must never create series: `model` is the model id the CLI reports when it ran (a bounded set of real ids), otherwise the requested model only if it is in `-claude-models` or is the default model, else `other`; `skill` is a loaded skill name or `other`; `outcome` and `type` are fixed enums. New labels must follow the same rule (normalize in `app/metrics.go`, test the normalizer). Observers are cheap, non-blocking and panic-safe; a metrics bug cannot fail a request or a sync.

### Traces

One request is one trace: the `otelhttp` server span (`POST /v1/chat/completions`) is the parent of `claude.run` (same trace id), streaming or not. Skill syncs are their own traces (`skills.sync`, started by the sync loop or by `POST /api/v1/skills/refresh`).

`claude.run` attributes: `claude.model` (requested, or the default), `claude.model.resolved`, `claude.outcome`, `claude.exit_code`, `claude.duration_ms`, `claude.queue_wait_ms`, `claude.ttft_ms` (only when text was produced), `claude.num_turns`, `claude.tokens.{input,output,cache_creation,cache_read}`, `claude.cost_usd`, `claude.skills.active` (bool, always set), `claude.skills.commit` (12-char commit of the snapshot used, when skills are active) and `claude.skills.invoked` (string array, only when the model used skills), `claude.web.search_requests` and `claude.web.fetch_requests` (ints, always set, 0 without web tools). Event `first_token` (attribute `claude.elapsed_ms`) marks the first text delta. Errors are recorded with `RecordError` and an error status. `skills.sync` attributes: `skills.repo`, `skills.ref`, `skills.commit`, `skills.outcome`, `skills.count`, `skills.skipped`. Attributes carry no prompt text. Span attributes are not limited to bounded values (unlike metric labels), so `claude.model` holds whatever the client sent.

### Forwarding the Claude Code CLI's own telemetry (optional)

The gateway builds each child's environment from its own (`os.Environ`, minus `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN`, see `filterEnv` in `services/claude/render.go`), so variables set on the gateway process reach `claude -p` with no code change. For example `CLAUDE_CODE_ENABLE_TELEMETRY=1` with `OTEL_METRICS_EXPORTER=otlp`, `OTEL_LOGS_EXPORTER=otlp` and `OTEL_EXPORTER_OTLP_ENDPOINT=...` makes every run export the CLI's metrics and log events. Prompt logging stays off unless you enable it explicitly (`OTEL_LOG_USER_PROMPTS=1`); do not enable it here, since prompts would leave the gateway. This duplicates part of the data above from a second source and adds one export per short-lived process; check the current CLI documentation for the exact variable names before relying on them. The container image sets `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`; override it with `-e` if you want the CLI's own telemetry.

## Operations

- **Scaling to N containers.** Run as many replicas as you like behind any load balancer: there is no session, memory or shared state, and every container needs only the same `CLAUDE_CODE_OAUTH_TOKEN` (and, optionally, the same API keys and skills repo). Each replica syncs skills on its own. See `compose.example.yaml`.
- **Concurrency.** `-claude-max-concurrency` (default 4) limits simultaneous `claude` processes **per container**; extra requests get `429 server_busy` with `Retry-After: 1` (raise the limit, or put a queue or retry in the client). Total parallelism is replicas times the limit. All containers draw on the **same Max usage limits**, so more replicas do not add capacity at Anthropic; hitting the limit shows up as `429 rate_limit_exceeded`. Budget roughly 300-500 MB of RAM per running process.
- **Timeouts.** `-claude-timeout` (default 5m) bounds a whole request (504 `upstream_timeout`; the CLI process group is killed). `-http-read-timeout` (10s) covers reading the request; the write timeout is off so streams are not cut.
- **Shutdown.** On SIGINT/SIGTERM both servers stop gracefully (30s), the skills syncer is cancelled, running CLI processes are bounded by their request context and the backend's temp directories are removed. Give the orchestrator a stop grace period of at least 30s.
- **Health.** `GET /api/v1/health` on the API port (public, no key); `claudeai-api -health-check [port]` (used by the image `HEALTHCHECK`; port defaults to 46564, connects over loopback) exits 0 when it answers 200. Health does not call Claude, so it cannot detect an expired token: alert on `claude_runs_total{outcome="auth"}` instead (see `grafana/alerts.example.yml`).
- **Token rotation.** Every year (or when revoked): `claude setup-token`, update the secret, restart the containers one by one.

## Troubleshooting

| Symptom | Likely cause and fix |
|---|---|
| `502 upstream_auth_error` | The OAuth token is missing, expired or revoked. Run `claude setup-token`, update `CLAUDE_CODE_OAUTH_TOKEN`, restart. Check that the container actually has the variable (`oauth_token_set=true` in the startup log) |
| `429 server_busy` | All `-claude-max-concurrency` slots are in use. Raise the limit (mind RAM and your usage limits), retry after `Retry-After`, or add a client-side queue |
| `429 rate_limit_exceeded` | Your Max usage limit is reached (shared with Claude Code and every other container using the login). Wait for the window to reset |
| `504 upstream_timeout` | The request exceeded `-claude-timeout`; raise it for very long generations |
| `404 model_not_found` | The model name is unknown to the CLI; use `sonnet`, `opus`, `haiku` or a full model id |
| Answers say "I have no internet access" / cannot check recent events | Web tools are off or unavailable. Check `-claude-web` is `search` or `full` (startup log field `claude_web`), that `claude_web_requests_total` grows after such a question, and that your account/model supports web search (the model says the search failed when it does not). Adding a skill alone does not help: only the flag grants the tool |
| Skills not loading | `curl -H "Authorization: Bearer <key>" http://127.0.0.1:46564/api/v1/skills` (omit the header when no keys are set): check `enabled`, `commit`, `skipped` reasons and the sync error in the logs. Typical causes: deploy key not readable by uid 1001 or not mode 0600/0400 (see `--userns=keep-id` above), `known_hosts` missing the git host, wrong `-skills-ref`, skill `name` not equal to its directory |
| Startup error about the skills key or `known_hosts` | The path passed to `-skills-ssh-key` / `-skills-known-hosts` does not exist inside the container; check the `:ro` mounts |
| `mkdir ... permission denied` for the skills dir | The state directory is not writable by uid 1001: use a named volume, or a tmpfs with `mode=1777` |
| Streaming stalls or arrives in one burst behind a proxy | Disable proxy buffering and raise the read timeout (nginx: `proxy_buffering off; proxy_read_timeout 300s;`); there is no heartbeat |
| Warning "API is unauthenticated and reachable beyond localhost" | Auth is off and the bind is not loopback. In a container this is expected (it binds `0.0.0.0`): publish the port on `127.0.0.1` only, or set `CLAUDEAI_API_KEYS` |
| Container shows no health status | The image was built with podman's default OCI format, which drops `HEALTHCHECK`: build with `--format docker` (`make container`) |

## Limits and non-goals

- Stateless by design: no conversation storage, sessions or memory; the client sends the full history.
- No tool/function calling, no images or audio, no `n` > 1, no logprobs; sampling parameters are ignored.
- Streaming has no heartbeat; usage numbers sum every turn of the run.
- Not a multi-user service: one subscription, personal use, concurrency-limited.

## Security notes

- **Secrets** are environment variables only (never flags, never logged). Keep `.env` mode 600 and out of git; prefer your orchestrator's secret store.
- **Bind addresses** default to loopback. The container binds `0.0.0.0`; publish the port on `127.0.0.1` or behind a TLS reverse proxy with `CLAUDEAI_API_KEYS` set.
- **One port for everything.** `/metrics` and `POST /api/v1/skills/refresh` share the API port. Without `CLAUDEAI_API_KEYS` they are open to anyone who can reach the port, so publish it on `127.0.0.1` only; with keys set they require the Bearer key. Only health, version and the Swagger UI are always public (the UI itself needs a key only to call protected operations).
- **Claude isolation.** Each run is a fresh `claude -p` with `--setting-sources`, `--strict-mcp-config`, `--no-chrome` and the smallest tool set (`--tools ""`; `Skill` when skills are active; `WebSearch`/`WebFetch` only per `-claude-web`), `ANTHROPIC_API_KEY` stripped, in its own process group killed on timeout.
- **Deploy key** is read-only and scoped to the one skills repository; a leaked key allows reading that repository only. Mount it `:ro`.
- **Skills trust model.** The skills repository is trusted input: its skill text is given to the model, so a compromised repository can inject prompts. The model has no shell or file tools, only `Skill` and, per `-claude-web`, the web tools, which bounds what an injected prompt can do (with `full`, `WebFetch` can reach URLs from the gateway host; see [Web access](#web-access)), but it can shape answers. Review skill changes like code. The syncer never executes repository content (bare clone, `ls-tree`/`cat-file`, no checkout, hooks or symlinks).
- The CLI reads project settings from its working directory **and its ancestors**: keep `-skills-dir` in a dedicated directory with no `CLAUDE.md` or `.claude/settings*` above it (the image uses `/var/lib/claudeai-api/skills`).

## Development

```bash
make run               # go run ./cmd on loopback
make test              # go test -race -count=1 ./...
make coverage          # same suite with a coverage profile (coverage.out); prints the total
make swagger           # regenerate docs/ after changing handler annotations (swag pinned in go.mod)
make check-swagger     # fails if docs/ is stale (what CI runs)
make vulncheck         # govulncheck ./... (install govulncheck separately)
make ci                # the CI checks locally, without modifying files (all but the container build)
make build             # vet + fmt + tidy + swagger check + build -> build/main (modifies files)
make container         # podman build --format docker -> claudeai-api:latest (CLAUDE_VERSION=x.y.z to re-pin the CLI)
make container-run     # run the image on 127.0.0.1:46564 using ./.env
```

Tests use only the standard library (`testing`, `httptest`, table-driven) and a re-exec **fake `claude` executable**: they need no real CLI, network or secrets, so CI runs on a plain GitHub runner (only `git` is used, by the skills syncer tests). Opt-in real-world tests: `CLAUDEAI_INTEGRATION=1` (real CLI) and `CLAUDEAI_SKILLS_E2E=1` with `CLAUDEAI_SKILLS_E2E_REPO/_KEY/_KNOWN_HOSTS` (real skills repo).

**CI** (`.github/workflows/ci.yml`): vet, gofmt, govulncheck, `go mod tidy -diff`, Swagger drift check, build, `go test -race` with coverage (the generated `docs` package is excluded), then a container build with a smoke test of `claude`, `git` and `ssh` in the image. On pushes to `main` the `coverage-badge` job publishes `coverage.json` to the orphan `badges` branch for the badge. **Release** (`release.yml`) triggers on tags `vX.Y.Z` (`git tag v0.1.0 && git push origin v0.1.0`): runs the checks, pushes the image to `ghcr.io/<owner>/<repo>` with tags `vX.Y.Z`, `X.Y`, `X`, `latest` and creates a GitHub release. `vulncheck.yml` runs govulncheck weekly. The Go version comes from the `go` directive in `go.mod`; the Containerfile builder image (`golang:1.27.2-bookworm`) must be bumped together with it.

**Adding a metric.** Create the instrument in `o11/metrics_claude.go` (or the relevant factory) with a bounded label set, feed it from the adapters in `app/metrics.go`, add it to the exposition test `o11/metrics_claude_test.go`, add a panel (and, if relevant, an alert) in `grafana/`, document it in the table above and keep `go test ./grafana/` green.

Architecture rules and conventions for contributors and coding agents are in [AGENTS.md](AGENTS.md) and [CLAUDE.md](CLAUDE.md).

## License

See [LICENSE](LICENSE).
