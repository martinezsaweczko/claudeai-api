package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// LoggingMiddleware creates a middleware that logs all HTTP requests
// Logs include: method, path, status code, response time, and request size
func LoggingMiddleware(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Wrap the response writer to capture status code and response size
			wrapped := &wrappedResponseWriter{
				ResponseWriter: w,
				statusCode:     http.StatusOK, // default status code
			}

			// Call the next handler
			next.ServeHTTP(wrapped, r)

			// Calculate request duration
			duration := time.Since(start)

			// Log the request
			log.Info(
				"HTTP request",
				"method", r.Method,
				"path", r.RequestURI,
				"status_code", wrapped.statusCode,
				"response_size", wrapped.written,
				"duration_ms", duration.Milliseconds(),
				"remote_addr", r.RemoteAddr,
			)
		})
	}
}

// wrappedResponseWriter wraps http.ResponseWriter to capture status code and size
type wrappedResponseWriter struct {
	http.ResponseWriter
	statusCode int
	written    int64
}

func (w *wrappedResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *wrappedResponseWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.written += int64(n)
	return n, err
}

// Ensure wrappedResponseWriter implements http.ResponseWriter
var _ http.ResponseWriter = (*wrappedResponseWriter)(nil)
