package handlers

import (
	"net/http"
	"testing"

	"github.com/martinezsaweczko/whatsappBot-golang/http/middleware"
	"log/slog"
	"os"
)

type swaggerRouter struct {
	routes map[string]bool
}

func (r *swaggerRouter) Handle(pattern string, _ http.Handler) {
	r.routes[pattern] = true
}

func (r *swaggerRouter) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	r.routes[pattern] = true
}

func (r *swaggerRouter) HandleWithMiddleware(pattern string, _ http.Handler, _ ...middleware.Middleware) {
	r.routes[pattern] = true
}

func (r *swaggerRouter) HandleFuncWithMiddleware(pattern string, _ func(http.ResponseWriter, *http.Request), _ ...middleware.Middleware) {
	r.routes[pattern] = true
}

func TestSwaggerRoutes(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	h := NewSwaggerHandler(log, "")

	router := &swaggerRouter{routes: make(map[string]bool)}
	h.RegisterRoutes(router)

	for _, path := range []string{"/docs", "/docs/"} {
		if !router.routes[path] {
			t.Errorf("expected route %s to be registered", path)
		}
	}
}

func TestSwaggerRoutesWithBasePath(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	h := NewSwaggerHandler(log, "/api/v1")

	router := &swaggerRouter{routes: make(map[string]bool)}
	h.RegisterRoutes(router)

	for _, path := range []string{"/api/v1/docs", "/api/v1/docs/"} {
		if !router.routes[path] {
			t.Errorf("expected route %s to be registered", path)
		}
	}
}
