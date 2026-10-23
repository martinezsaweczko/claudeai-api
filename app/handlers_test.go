package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/martinezsaweczko/claudeai-api/config"
	"github.com/martinezsaweczko/claudeai-api/http/middleware"
	"github.com/martinezsaweczko/claudeai-api/model"
	"github.com/martinezsaweczko/claudeai-api/o11"
	"github.com/martinezsaweczko/claudeai-api/services/claude"
	"go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// muxServer is a Server backed by a ServeMux.
type muxServer struct{ mux *http.ServeMux }

func newMuxServer() *muxServer { return &muxServer{mux: http.NewServeMux()} }

func (s *muxServer) Start() error                    { return nil }
func (s *muxServer) Stop(context.Context) error      { return nil }
func (s *muxServer) Handle(p string, h http.Handler) { s.mux.Handle(p, h) }
func (s *muxServer) HandleFunc(p string, h func(http.ResponseWriter, *http.Request)) {
	s.mux.HandleFunc(p, h)
}
func (s *muxServer) HandleWithMiddleware(p string, h http.Handler, mws ...middleware.Middleware) {
	s.mux.Handle(p, middleware.Chain(h, mws...))
}
func (s *muxServer) HandleFuncWithMiddleware(p string, h func(http.ResponseWriter, *http.Request), mws ...middleware.Middleware) {
	s.mux.Handle(p, middleware.Chain(http.HandlerFunc(h), mws...))
}

type stubBackend struct{ calls int }

func (b *stubBackend) Stream(context.Context, claude.Request, func(claude.Event) error) (claude.Result, error) {
	return claude.Result{}, nil
}
func (b *stubBackend) Complete(context.Context, claude.Request) (claude.Result, error) {
	b.calls++
	return claude.Result{Text: "OK", Model: "m"}, nil
}

func wiredServers(t *testing.T, keys []string) (public, internal *muxServer, be *stubBackend) {
	t.Helper()
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	cfg := &config.Config{Claude: config.ClaudeConfig{
		Bin: "claude", Model: "sonnet", Models: []string{"sonnet", "opus"},
		Timeout: time.Minute, MaxConcurrency: 1, APIKeys: keys,
	}}
	public, internal, be = newMuxServer(), newMuxServer(), &stubBackend{}
	a := NewApp(cfg).
		WithPublicServer(public).
		WithInternalServer(internal).
		WithLog(slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithObservability(&o11.ObservabilityInst{Trace: tp, MeterProvider: noop.NewMeterProvider()}).
		WithBuildInfo(model.BuildInfo{Version: "test"}).
		WithBasePath("/api/v1")
	a.lookupEnv = func(string) (string, bool) { return "", false }
	a.claude = be
	if err := a.initAndWire(); err != nil {
		t.Fatalf("initAndWire: %v", err)
	}
	return public, internal, be
}

func do(s *muxServer, method, path, auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

const chatBody = `{"model":"haiku","messages":[{"role":"user","content":"hi"}]}`

func TestV1RoutesAndAPIKeyAuth(t *testing.T) {
	t.Run("keys configured", func(t *testing.T) {
		public, _, be := wiredServers(t, []string{"secret"})
		tests := []struct {
			name, method, path, auth, body string
			want                           int
		}{
			{"models without key", "GET", "/v1/models", "", "", 401},
			{"models wrong key", "GET", "/v1/models", "Bearer nope", "", 401},
			{"models right key", "GET", "/v1/models", "Bearer secret", "", 200},
			{"model detail right key", "GET", "/v1/models/opus", "Bearer secret", "", 200},
			{"chat without key", "POST", "/v1/chat/completions", "", chatBody, 401},
			{"chat right key", "POST", "/v1/chat/completions", "Bearer secret", chatBody, 200},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if rec := do(public, tc.method, tc.path, tc.auth, tc.body); rec.Code != tc.want {
					t.Errorf("status = %d, want %d, body %s", rec.Code, tc.want, rec.Body.String())
				}
			})
		}
		if be.calls != 1 {
			t.Errorf("backend calls = %d, want 1 (only the authenticated chat)", be.calls)
		}
	})

	t.Run("no keys configured", func(t *testing.T) {
		public, _, _ := wiredServers(t, nil)
		if rec := do(public, "GET", "/v1/models", "", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"opus"`) {
			t.Errorf("models = %d %s", rec.Code, rec.Body.String())
		}
		if rec := do(public, "POST", "/v1/chat/completions", "", chatBody); rec.Code != 200 {
			t.Errorf("chat = %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("internal endpoints are not behind the key and v1 is public-only", func(t *testing.T) {
		public, internal, _ := wiredServers(t, []string{"secret"})
		for _, p := range []string{"/api/v1/health", "/api/v1/version"} {
			if rec := do(internal, "GET", p, "", ""); rec.Code != 200 {
				t.Errorf("internal %s = %d, want 200", p, rec.Code)
			}
		}
		if rec := do(internal, "GET", "/v1/models", "", ""); rec.Code != 404 {
			t.Errorf("/v1/models on the internal server = %d, want 404", rec.Code)
		}
		if rec := do(public, "GET", "/api/v1/health", "", ""); rec.Code != 404 {
			t.Errorf("health on the public server = %d, want 404", rec.Code)
		}
	})
}
