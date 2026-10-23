package middleware

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// TracingMiddleware creates a middleware that starts a server span per request.
// Spans are named "{method} {route}" (OTel semconv), e.g. "GET /items/{id}".
func TracingMiddleware() Middleware {
	return func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(next, "http.server",
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				return r.Method + " " + spanRoute(r)
			}),
		)
	}
}

// spanRoute returns the matched mux pattern without its optional method prefix,
// falling back to the request path when the request was not routed by a mux.
func spanRoute(r *http.Request) string {
	if r.Pattern == "" {
		return r.URL.Path
	}
	if _, path, found := strings.Cut(r.Pattern, " "); found {
		return path
	}
	return r.Pattern
}
