package http_server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/martinezsaweczko/whatsappBot-golang/http/handlers"
)

func TestSwaggerRoutesRegistered(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := &HTTPServerConfig{
		Addr:         "127.0.0.1",
		Port:         9000,
		Timeout:      30,
		ReadTimeout:  10,
		WriteTimeout: 30,
		Log:          log,
	}
	server := cfg.New()

	swagger := handlers.NewSwaggerHandler(log, "")
	swagger.RegisterRoutes(server)

	// Use the underlying mux with httptest to exercise the real routing.
	ts := httptest.NewServer(server.mux)
	defer ts.Close()

	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	resp, err := client.Get(ts.URL + "/docs")
	if err != nil {
		t.Fatalf("http get failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("redirect status = %d, want %d", resp.StatusCode, http.StatusMovedPermanently)
	}
	loc := resp.Header.Get("Location")
	if loc != "/docs/" {
		t.Errorf("location = %q, want /docs/", loc)
	}

	ui, err := client.Get(ts.URL + "/docs/index.html")
	if err != nil {
		t.Fatalf("http get failed: %v", err)
	}
	defer ui.Body.Close()
	if ui.StatusCode != http.StatusOK {
		t.Fatalf("ui status = %d, want %d", ui.StatusCode, http.StatusOK)
	}
	ct := ui.Header.Get("Content-Type")
	if ct != "text/html; charset=utf-8" {
		t.Errorf("content-type = %q, want text/html; charset=utf-8", ct)
	}
}
