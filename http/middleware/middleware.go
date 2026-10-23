package middleware

import "net/http"

// Middleware defines the middleware function signature
// It takes a handler and returns a wrapped handler
type Middleware func(http.Handler) http.Handler

// Chain applies multiple middlewares to a handler in order
// Middlewares are applied from first to last, so the first middleware
// will be the outermost layer of the chain
// Example: Chain(handler, loggingMiddleware, authMiddleware)
// Results in: auth -> logging -> handler
func Chain(handler http.Handler, middlewares ...Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		handler = middlewares[i](handler)
	}
	return handler
}

// HandlerFunc is a convenience function to wrap http.HandlerFunc with middleware
func HandlerFunc(handler http.HandlerFunc, middlewares ...Middleware) http.HandlerFunc {
	wrapped := Chain(handler, middlewares...)
	return func(w http.ResponseWriter, r *http.Request) {
		wrapped.ServeHTTP(w, r)
	}
}
