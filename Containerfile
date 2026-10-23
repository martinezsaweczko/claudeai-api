# Builder stage
FROM golang:1.27-bookworm AS builder

WORKDIR /app

# Copy dependency files first for layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build static binary (pure Go, no CGO needed)
ARG VERSION=dev
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-X main.version=${VERSION} -X main.buildTime=${BUILD_TIME} -X main.appName=claudeai-api -s -w" \
    -o /out/claudeai-api cmd/main.go

# Runtime stage - minimal image
FROM debian:bookworm-slim

# ca-certificates: outbound HTTPS calls
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Non-root user
RUN groupadd -g 1001 appuser && \
    useradd -m -u 1001 -g appuser appuser

COPY --from=builder /out/claudeai-api /usr/local/bin/claudeai-api

USER appuser

# Internal API health check
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/claudeai-api", "-health-check"] || exit 1

ENTRYPOINT ["/usr/local/bin/claudeai-api"]
