package app

import (
	"context"
	"encoding/json"
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

// wiredServer wires the whole app on ONE mux, like production.
func wiredServer(t *testing.T, keys []string) (*muxServer, *stubBackend) {
	t.Helper()
	be := &stubBackend{}
	return wiredServerWith(t, keys, be), be
}

// wiredServerWith wires the app around the given Claude backend.
func wiredServerWith(t *testing.T, keys []string, backend claude.Backend) *muxServer {
	t.Helper()
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	cfg := &config.Config{
		Claude: config.ClaudeConfig{
			Bin: "claude", Model: "sonnet", Models: []string{"sonnet", "opus"},
			Timeout: time.Minute, MaxConcurrency: 1, APIKeys: keys,
		},
		O11: config.O11Config{PrometheusPath: "/metrics"},
	}
	srv := newMuxServer()
	a := NewApp(cfg).
		WithServer(srv).
		WithLog(slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithObservability(&o11.ObservabilityInst{Trace: tp, MeterProvider: noop.NewMeterProvider()}).
		WithBuildInfo(model.BuildInfo{Version: "test"}).
		WithBasePath("/api/v1")
	a.lookupEnv = func(string) (string, bool) { return "", false }
	a.claude = backend
	if err := a.initAndWire(); err != nil {
		t.Fatalf("initAndWire: %v", err)
	}
	return srv
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
		srv, be := wiredServer(t, []string{"secret"})
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
				if rec := do(srv, tc.method, tc.path, tc.auth, tc.body); rec.Code != tc.want {
					t.Errorf("status = %d, want %d, body %s", rec.Code, tc.want, rec.Body.String())
				}
			})
		}
		if be.calls != 1 {
			t.Errorf("backend calls = %d, want 1 (only the authenticated chat)", be.calls)
		}
	})

	t.Run("no keys configured", func(t *testing.T) {
		srv, _ := wiredServer(t, nil)
		if rec := do(srv, "GET", "/v1/models", "", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"opus"`) {
			t.Errorf("models = %d %s", rec.Code, rec.Body.String())
		}
		if rec := do(srv, "POST", "/v1/chat/completions", "", chatBody); rec.Code != 200 {
			t.Errorf("chat = %d %s", rec.Code, rec.Body.String())
		}
	})
}

func TestInitAndWireRequiresObservability(t *testing.T) {
	cfg := &config.Config{Claude: config.ClaudeConfig{
		Bin: "claude", Model: "sonnet", Models: []string{"sonnet"},
		Timeout: time.Minute, MaxConcurrency: 1,
	}}
	a := NewApp(cfg).
		WithServer(newMuxServer()).
		WithLog(slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithBuildInfo(model.BuildInfo{Version: "test"}).
		WithBasePath("/api/v1")
	a.lookupEnv = func(string) (string, bool) { return "", false }
	a.claude = &stubBackend{}

	if err := a.initAndWire(); err == nil {
		t.Fatal("initAndWire without observability: want an error, got nil")
	}
}

// authMatrixCase is one row of the single-port auth matrix.
type authMatrixCase struct {
	method, path string
	protected    bool
	okStatus     int // expected status with the right key (or without keys)
}

func authMatrix() []authMatrixCase {
	return []authMatrixCase{
		{"GET", "/v1/models", true, 200},
		{"GET", "/v1/models/opus", true, 200},
		{"POST", "/v1/chat/completions", true, 200},
		{"GET", "/metrics", true, 200},
		{"GET", "/api/v1/skills", true, 200},
		{"POST", "/api/v1/skills/refresh", true, 404}, // skills disabled in this wiring
		{"GET", "/api/v1/health", false, 200},
		{"GET", "/api/v1/version", false, 200},
		{"GET", "/api/v1/docs/index.html", false, 200},
		{"GET", "/api/v1/docs/doc.json", false, 200},
		{"GET", "/docs", false, 302},
		{"GET", "/docs/", false, 302},
	}
}

func TestSinglePortAuthMatrix(t *testing.T) {
	body := func(c authMatrixCase) string {
		if c.path == "/v1/chat/completions" {
			return chatBody
		}
		return ""
	}

	t.Run("keys configured", func(t *testing.T) {
		srv, _ := wiredServer(t, []string{"secret", "other"})
		for _, c := range authMatrix() {
			t.Run(c.method+" "+c.path, func(t *testing.T) {
				if c.protected {
					for _, auth := range []string{"", "Bearer nope", "Basic secret", "secret"} {
						if rec := do(srv, c.method, c.path, auth, body(c)); rec.Code != 401 {
							t.Errorf("auth %q = %d, want 401", auth, rec.Code)
						}
					}
					rec := do(srv, c.method, c.path, "", body(c))
					if rec.Header().Get("WWW-Authenticate") != "Bearer" {
						t.Errorf("missing WWW-Authenticate challenge")
					}
					for _, key := range []string{"secret", "other"} {
						if rec := do(srv, c.method, c.path, "Bearer "+key, body(c)); rec.Code != c.okStatus {
							t.Errorf("right key = %d, want %d: %s", rec.Code, c.okStatus, rec.Body.String())
						}
					}
					return
				}
				if rec := do(srv, c.method, c.path, "", body(c)); rec.Code != c.okStatus {
					t.Errorf("public endpoint without key = %d, want %d", rec.Code, c.okStatus)
				}
			})
		}
	})

	t.Run("no keys configured", func(t *testing.T) {
		srv, _ := wiredServer(t, nil)
		for _, c := range authMatrix() {
			if rec := do(srv, c.method, c.path, "", body(c)); rec.Code != c.okStatus {
				t.Errorf("%s %s without keys = %d, want %d", c.method, c.path, rec.Code, c.okStatus)
			}
		}
	})

	t.Run("redirect targets the Swagger UI", func(t *testing.T) {
		srv, _ := wiredServer(t, []string{"secret"})
		rec := do(srv, "GET", "/docs", "", "")
		if loc := rec.Header().Get("Location"); loc != "/api/v1/docs/index.html" {
			t.Errorf("Location = %q", loc)
		}
	})
}

func TestMergedSpecDescribesEverything(t *testing.T) {
	srv, _ := wiredServer(t, nil)
	rec := do(srv, "GET", "/api/v1/docs/doc.json", "", "")
	spec := rec.Body.String()
	for _, want := range []string{`"/v1/chat/completions"`, `"/v1/models"`, `"/api/v1/skills"`, `"/api/v1/skills/refresh"`, `"/api/v1/health"`, `"/metrics"`, `"BearerAuth"`} {
		if !strings.Contains(spec, want) {
			t.Errorf("spec lacks %s", want)
		}
	}
	if strings.Contains(spec, `"host": "localhost`) || strings.Contains(spec, `"host": "127`) {
		t.Error("spec pins a host: Swagger would call another origin")
	}

	// Every protected operation advertises BearerAuth so the Authorize button works.
	var doc struct {
		Paths map[string]map[string]struct {
			Security []map[string][]string `json:"security"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("doc.json: %v", err)
	}
	for _, c := range authMatrix() {
		op := doc.Paths[strings.TrimSuffix(c.path, "/")][strings.ToLower(c.method)]
		if c.path == "/v1/models/opus" {
			op = doc.Paths["/v1/models/{model}"]["get"]
		}
		if !c.protected {
			continue
		}
		if len(op.Security) == 0 || op.Security[0]["BearerAuth"] == nil {
			t.Errorf("%s %s lacks @Security BearerAuth in the spec", c.method, c.path)
		}
	}
}
