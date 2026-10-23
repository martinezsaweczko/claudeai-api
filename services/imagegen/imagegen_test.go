package imagegen

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func TestGenerate(t *testing.T) {
	imageBytes := []byte("fake-jpeg")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.FormValue("prompt") != "un gato" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"image":"` + base64.StdEncoding.EncodeToString(imageBytes) + `","finish_reason":"SUCCESS","seed":1}`))
	}))
	defer server.Close()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc, err := New(server.Client(), "test-key", log, tracenoop.NewTracerProvider(), noop.NewMeterProvider())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// Redirect the const endpoint to the test server
	svc.client = &http.Client{
		Transport: &hostRewriteTransport{scheme: "http", host: strings.TrimPrefix(server.URL, "http://"), base: http.DefaultTransport},
	}

	got, err := svc.Generate(context.Background(), "un gato")
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	if string(got) != string(imageBytes) {
		t.Errorf("unexpected image bytes: %q", got)
	}
}

func TestGenerateEmpty(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc, _ := New(http.DefaultClient, "key", log, tracenoop.NewTracerProvider(), noop.NewMeterProvider())
	if _, err := svc.Generate(context.Background(), "  "); err == nil {
		t.Fatal("expected error for empty prompt")
	}
}

func TestGenerateAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc, _ := New(http.DefaultClient, "key", log, tracenoop.NewTracerProvider(), noop.NewMeterProvider())
	svc.client = &http.Client{
		Transport: &hostRewriteTransport{scheme: "http", host: strings.TrimPrefix(server.URL, "http://"), base: http.DefaultTransport},
	}

	if _, err := svc.Generate(context.Background(), "test"); err == nil {
		t.Fatal("expected error for API failure")
	}
}

// hostRewriteTransport rewrites all requests to the test server (endpoint is a const)
type hostRewriteTransport struct {
	scheme string
	host   string
	base   http.RoundTripper
}

func (t *hostRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = t.scheme
	req.URL.Host = t.host
	return t.base.RoundTrip(req)
}
