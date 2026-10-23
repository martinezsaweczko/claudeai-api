package middleware

import (
	"net/http"
	"time"

	"github.com/martinezsaweczko/whatsappBot-golang/o11"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// MetricsMiddleware creates a middleware that records RED metrics
// (requests total, duration, in-flight) for every HTTP request
func MetricsMiddleware(m *o11.HTTPMetrics) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			route := r.Pattern
			if route == "" {
				route = "unknown"
			}

			m.InFlight.Add(r.Context(), 1)
			defer m.InFlight.Add(r.Context(), -1)

			wrapped := &wrappedResponseWriter{
				ResponseWriter: w,
				statusCode:     http.StatusOK,
			}

			next.ServeHTTP(wrapped, r)

			duration := time.Since(start).Seconds()
			attrs := metric.WithAttributes(
				attribute.String("route", route),
				attribute.String("method", r.Method),
				attribute.Int("status", wrapped.statusCode),
			)
			m.RequestsTotal.Add(r.Context(), 1, attrs)
			m.RequestDuration.Record(r.Context(), duration, metric.WithAttributes(
				attribute.String("route", route),
				attribute.String("method", r.Method),
			))
		})
	}
}
