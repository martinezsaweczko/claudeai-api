package middleware

import (
	"context"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// maxRecordedUses mirrors the Node.js behavior: a token is accepted while its
// recorded use count is <= 2 (i.e. up to 3 downloads per token)
const maxRecordedUses = 2

// TokenStore records JWT usages for replay protection (consumer-defined interface)
type TokenStore interface {
	CountTokenUses(ctx context.Context, jwt string) (int, error)
	SaveToken(ctx context.Context, jwt string) error
	CleanJWT(ctx context.Context) error
}

// UsageReporter records file access attempts (consumer-defined interface)
type UsageReporter interface {
	ReportFileUsage(ctx context.Context, file string, result int) error
}

// JWTQueryMiddleware creates a middleware that authenticates requests with a JWT
// passed in the ?access_token= query parameter, with replay protection backed by
// the TokenStore. Rejected requests are reported and measured.
func JWTQueryMiddleware(log *slog.Logger, verify func(token string) error, store TokenStore, usage UsageReporter, rejected metric.Int64Counter) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			file := r.PathValue("file")

			token := r.URL.Query().Get("access_token")
			if token == "" {
				reject(ctx, w, log, usage, rejected, file, "missing token")
				return
			}

			if err := verify(token); err != nil {
				log.Warn("Invalid download token", "error", err)
				reject(ctx, w, log, usage, rejected, file, "invalid")
				return
			}

			uses, err := store.CountTokenUses(ctx, token)
			if err != nil {
				log.Error("Failed to count token uses", "error", err)
				reject(ctx, w, log, usage, rejected, file, "invalid")
				return
			}
			if uses > maxRecordedUses {
				log.Warn("Token replay limit reached", "file", file, "uses", uses)
				reject(ctx, w, log, usage, rejected, file, "replay")
				return
			}

			if err := store.SaveToken(ctx, token); err != nil {
				log.Error("Failed to record token usage", "error", err)
			}
			if err := store.CleanJWT(ctx); err != nil {
				log.Error("Failed to clean old JWTs", "error", err)
			}

			next.ServeHTTP(w, r)
		})
	}
}

// reject writes a 401 response, reports the failed attempt and counts the rejection
func reject(ctx context.Context, w http.ResponseWriter, log *slog.Logger, usage UsageReporter, rejected metric.Int64Counter, file, reason string) {
	rejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
	if err := usage.ReportFileUsage(ctx, file, http.StatusUnauthorized); err != nil {
		log.Error("Failed to report file usage", "error", err)
	}
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte("401 Unauthorized\n"))
}
