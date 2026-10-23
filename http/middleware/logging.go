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
			// The request context carries the server span (TracingMiddleware runs
			// outside this one), so trace_id/span_id are added to the line.
			log.InfoContext(
				r.Context(),
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

// Flush implements http.Flusher by delegating to the underlying writer when it
// supports flushing. It is required for streaming (SSE) responses.
func (w *wrappedResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying writer so http.NewResponseController can reach
// optional interfaces such as deadlines and hijacking.
func (w *wrappedResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Ensure wrappedResponseWriter implements the optional writer interfaces
var (
	_ http.ResponseWriter = (*wrappedResponseWriter)(nil)
	_ http.Flusher        = (*wrappedResponseWriter)(nil)
)
