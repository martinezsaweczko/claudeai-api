# Builder stage
FROM golang:1.27-bookworm AS builder

WORKDIR /app

# Copy dependency files first for layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build static binary (modernc.org/sqlite is pure Go, no CGO needed)
ARG VERSION=dev
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-X main.version=${VERSION} -X main.buildTime=${BUILD_TIME} -X main.appName=whatsappbot-golang -s -w" \
    -o /out/whatsappbot cmd/main.go

# Runtime stage - minimal image
FROM debian:bookworm-slim

# ca-certificates: HTTPS calls (WhatsApp, OpenAI, ESIOS...)
# tzdata: Europe/Madrid timezone for the electricity price window
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    tzdata \
    && rm -rf /var/lib/apt/lists/*

# Non-root user
RUN groupadd -g 1001 botuser && \
    useradd -m -u 1001 -g botuser botuser

COPY --from=builder /out/whatsappbot /usr/local/bin/whatsappbot

USER botuser

# Internal API health check
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/whatsappbot", "-health-check"] || exit 1

ENTRYPOINT ["/usr/local/bin/whatsappbot"]
