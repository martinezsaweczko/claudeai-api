package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/martinezsaweczko/whatsappBot-golang/http/middleware"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

type fakeFileUsage struct {
	reports map[string]int
}

func (f *fakeFileUsage) ReportFileUsage(_ context.Context, file string, result int) error {
	if f.reports == nil {
		f.reports = map[string]int{}
	}
	f.reports[file] = result
	return nil
}

func newTestFileServer(t *testing.T, dir string) (*FileServerHandler, *fakeFileUsage) {
	t.Helper()
	usage := &fakeFileUsage{}
	conf := FileServerHandlerConfig{
		Log:        slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		TP:         tracenoop.NewTracerProvider(),
		MP:         noop.NewMeterProvider(),
		Usage:      usage,
		Categories: map[string]string{"nacional_folder": dir},
	}
	h, err := conf.NewFileServerHandler()
	if err != nil {
		t.Fatalf("failed to create handler: %v", err)
	}
	return h, usage
}

func TestServeFileOK(t *testing.T) {
	dir := t.TempDir()
	content := []byte("fake pdf content")
	if err := os.WriteFile(filepath.Join(dir, "paper.pdf"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	h, usage := newTestFileServer(t, dir)
	handler := h.serveFile("nacional_folder", dir)

	req := httptest.NewRequest(http.MethodGet, "/nacional_folder/paper.pdf?access_token=x", nil)
	req.SetPathValue("file", "paper.pdf")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("Content-Disposition") != "attachment" {
		t.Errorf("missing attachment disposition: %v", rec.Header())
	}
	if rec.Header().Get("Content-Type") != "application/pdf" {
		t.Errorf("unexpected content type: %s", rec.Header().Get("Content-Type"))
	}
	if rec.Body.String() != string(content) {
		t.Errorf("unexpected body: %q", rec.Body.String())
	}
	if usage.reports["paper.pdf"] != http.StatusOK {
		t.Errorf("200 not reported: %+v", usage.reports)
	}
}

func TestServeFileNotFound(t *testing.T) {
	dir := t.TempDir()
	h, usage := newTestFileServer(t, dir)
	handler := h.serveFile("nacional_folder", dir)

	req := httptest.NewRequest(http.MethodGet, "/nacional_folder/missing.pdf", nil)
	req.SetPathValue("file", "missing.pdf")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if usage.reports["missing.pdf"] != http.StatusNotFound {
		t.Errorf("404 not reported: %+v", usage.reports)
	}
}

func TestServeFilePathTraversal(t *testing.T) {
	dir := t.TempDir()
	h, _ := newTestFileServer(t, dir)
	handler := h.serveFile("nacional_folder", dir)

	req := httptest.NewRequest(http.MethodGet, "/nacional_folder/..%2Fsecret", nil)
	req.SetPathValue("file", "../secret")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("traversal should yield 404, got %d", rec.Code)
	}
}

func TestRegisterRoutes(t *testing.T) {
	dir := t.TempDir()
	h, _ := newTestFileServer(t, dir)

	server := newFakeRouter()
	h.RegisterRoutes(server)

	if len(server.routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(server.routes))
	}
	if server.routes[0] != "GET /nacional_folder/{file}" {
		t.Errorf("unexpected route: %s", server.routes[0])
	}
}

// fakeRouter implements the Router interface for tests
type fakeRouter struct {
	routes []string
}

func newFakeRouter() *fakeRouter { return &fakeRouter{} }

func (f *fakeRouter) Handle(pattern string, _ http.Handler) {
	f.routes = append(f.routes, pattern)
}
func (f *fakeRouter) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	f.routes = append(f.routes, pattern)
}
func (f *fakeRouter) HandleWithMiddleware(pattern string, _ http.Handler, _ ...middleware.Middleware) {
	f.routes = append(f.routes, pattern)
}
func (f *fakeRouter) HandleFuncWithMiddleware(pattern string, _ func(http.ResponseWriter, *http.Request), _ ...middleware.Middleware) {
	f.routes = append(f.routes, pattern)
}
