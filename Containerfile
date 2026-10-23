# Build with `podman build --format docker ...` (podman's default OCI format drops
# HEALTHCHECK); `make container` already does this. docker needs no flag.

# Claude Code CLI release pinned for reproducible builds. Override with
# --build-arg CLAUDE_VERSION=x.y.z (the manifest signature is checked either way).
ARG CLAUDE_VERSION=2.1.309

# ---------------------------------------------------------------------------
# Stage 1: fetch and verify the Claude Code CLI (official release binary, no
# Node.js). Verification chain, as documented at
# https://code.claude.com/docs/en/setup#binary-integrity-and-code-signing :
#   1. the release signing key is downloaded and its fingerprint must equal the
#      one pinned below (published in the docs);
#   2. the release manifest.json must carry a valid detached signature made by
#      exactly that key;
#   3. the binary's sha256 must equal the checksum listed in the signed manifest.
# Not verified: the TLS-delivered key itself is trusted only through the pinned
# fingerprint; Linux binaries carry no individual code signature.
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim AS claude-fetch

ARG CLAUDE_VERSION
ARG CLAUDE_SIGNING_FPR=31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl gnupg jq \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /dl
RUN set -eux; \
    base="https://downloads.claude.ai/claude-code-releases"; \
    case "$(dpkg --print-architecture)" in \
      amd64) platform=linux-x64 ;; \
      arm64) platform=linux-arm64 ;; \
      *) echo "unsupported architecture" >&2; exit 1 ;; \
    esac; \
    export GNUPGHOME="$(mktemp -d)"; \
    curl -fsSL https://downloads.claude.ai/keys/claude-code.asc -o key.asc; \
    test "$(gpg --show-keys --with-colons key.asc | awk -F: '/^fpr/ {print $10; exit}')" = "$CLAUDE_SIGNING_FPR"; \
    gpg --batch --import key.asc; \
    curl -fsSL "$base/$CLAUDE_VERSION/manifest.json" -o manifest.json; \
    curl -fsSL "$base/$CLAUDE_VERSION/manifest.json.sig" -o manifest.json.sig; \
    gpg --batch --status-fd 1 --verify manifest.json.sig manifest.json | grep -q "^\[GNUPG:\] VALIDSIG $CLAUDE_SIGNING_FPR "; \
    test "$(jq -r .version manifest.json)" = "$CLAUDE_VERSION"; \
    sum="$(jq -r --arg p "$platform" '.platforms[$p].checksum' manifest.json)"; \
    curl -fsSL "$base/$CLAUDE_VERSION/$platform/claude" -o claude; \
    echo "$sum  claude" | sha256sum -c -; \
    chmod 0755 claude

# ---------------------------------------------------------------------------
# Stage 2: build the gateway (pure Go, no CGO)
# ---------------------------------------------------------------------------
FROM golang:1.27.2-bookworm AS builder

WORKDIR /app

# Copy dependency files first for layer caching
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-X main.version=${VERSION} -X main.buildTime=${BUILD_TIME} -X main.appName=claudeai-api -s -w" \
    -o /out/claudeai-api cmd/main.go

# ---------------------------------------------------------------------------
# Stage 3: runtime
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim

# ca-certificates: outbound HTTPS (Claude API); git + openssh-client: the skills
# syncer clones the private skills repository over ssh.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    git \
    openssh-client \
    && rm -rf /var/lib/apt/lists/*

# Non-root user with a real, writable HOME: the Claude CLI writes its config and
# cache under $HOME even in -p mode. /var/lib/claudeai-api is the skills state.
RUN groupadd -g 1001 appuser && \
    useradd -m -d /home/appuser -s /usr/sbin/nologin -u 1001 -g appuser appuser && \
    mkdir -p /var/lib/claudeai-api && \
    chown appuser:appuser /var/lib/claudeai-api

COPY --from=claude-fetch /dl/claude /usr/local/bin/claude
COPY --from=builder /out/claudeai-api /usr/local/bin/claudeai-api

# DISABLE_UPDATES: the CLI must not replace itself (the version is pinned and the
# binary is root-owned). CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: no telemetry,
# error reporting or feedback traffic. Both are documented at
# https://code.claude.com/docs/en/env-vars and https://code.claude.com/docs/en/setup.
# No credentials are baked in: pass CLAUDE_CODE_OAUTH_TOKEN at run time.
ENV HOME=/home/appuser \
    DISABLE_UPDATES=1 \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1

USER appuser
WORKDIR /home/appuser

# Mount points (see README / compose.example.yaml):
#   /var/lib/claudeai-api  skills state (disposable cache; a volume or tmpfs, owned by uid 1001)
#   /run/secrets/          read-only deploy key + known_hosts, readable by uid 1001
# Only the public port is published; the internal port (9000: health, metrics,
# unauthenticated skills endpoints) stays on the container network.
EXPOSE 46564

# Internal API health check
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/claudeai-api", "-health-check"]

ENTRYPOINT ["/usr/local/bin/claudeai-api"]
# The container must listen on every interface so the published port works; the
# host-side default stays loopback. Publish with -p 127.0.0.1:46564:46564.
CMD ["-http-address", "0.0.0.0", "-internal-address", "0.0.0.0", "-skills-dir", "/var/lib/claudeai-api/skills"]
