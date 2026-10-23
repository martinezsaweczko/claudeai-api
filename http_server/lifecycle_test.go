package http_server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/martinezsaweczko/claudeai-api/http/middleware"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// errorLogHandler forwards the message of every error-level record to a channel.
type errorLogHandler struct {
	errors chan string
}

func (h *errorLogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelError
}

func (h *errorLogHandler) Handle(_ context.Context, r slog.Record) error {
	msg := r.Message
	r.Attrs(func(a slog.Attr) bool {
		msg += " " + a.Key + "=" + a.Value.String()
		return true
	})
	h.errors <- msg
	return nil
}

func (h *errorLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *errorLogHandler) WithGroup(string) slog.Handler      { return h }

// freePort asks the kernel for an unused loopback port and releases it.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve a port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("failed to release the port: %v", err)
	}
	return port
}

// getWhenListening issues GET requests until the server accepts one. Start
// gives no readiness signal (it listens in a goroutine), so the only way to
// observe it is to retry the connection, bounded by a deadline.
func getWhenListening(t *testing.T, url string) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(url)
		if err == nil {
			return resp
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never accepted a connection: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNewAppliesAddressAndTimeouts(t *testing.T) {
	server := (&HTTPServerConfig{Addr: "127.0.0.1", Port: 8123, Timeout: 30, ReadTimeout: 10, WriteTimeout: 20, Log: discardLogger()}).New()

	if got := server.httpServer.Addr; got != "127.0.0.1:8123" {
		t.Errorf("address = %q, want 127.0.0.1:8123", got)
	}
	if got := server.httpServer.IdleTimeout; got != 30*time.Second {
		t.Errorf("idle timeout = %v, want 30s", got)
	}
	if got := server.httpServer.ReadTimeout; got != 10*time.Second {
		t.Errorf("read timeout = %v, want 10s", got)
	}
	if got := server.httpServer.WriteTimeout; got != 20*time.Second {
		t.Errorf("write timeout = %v, want 20s", got)
	}
}

func TestStartServesRequestsUntilStopped(t *testing.T) {
	port := freePort(t)
	server := (&HTTPServerConfig{Addr: "127.0.0.1", Port: port, Timeout: 5, ReadTimeout: 5, WriteTimeout: 5, Log: discardLogger()}).New()
	server.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("pong"))
	})

	if err := server.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/ping", port)

	resp := getWhenListening(t, url)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("failed to read the response: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "pong" {
		t.Errorf("got %d %q, want 200 pong", resp.StatusCode, body)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Stop(ctx); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	// Shutdown closes the listener before returning.
	client := &http.Client{Timeout: time.Second}
	if resp, err := client.Get(url); err == nil {
		resp.Body.Close()
		t.Error("the server still answers after Stop")
	}
}

// A bind failure must be returned by Start so the caller can abort startup.
func TestStartReturnsAnErrorWhenThePortIsTaken(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to occupy a port: %v", err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port

	server := (&HTTPServerConfig{Addr: "127.0.0.1", Port: port, Log: discardLogger()}).New()

	err = server.Start()
	if err == nil {
		t.Fatal("Start = nil, want a bind error when the port is taken")
	}
	if !strings.Contains(err.Error(), "address already in use") {
		t.Errorf("Start error = %v, want it to mention the address already in use", err)
	}

	if err := server.Stop(context.Background()); err != nil {
		t.Errorf("Stop after a failed bind = %v, want nil", err)
	}
}

func TestStopWithoutAnUnderlyingServer(t *testing.T) {
	if err := (&HTTPServer{}).Stop(context.Background()); err != nil {
		t.Errorf("Stop on an empty server = %v, want nil", err)
	}
}

// headerMiddleware appends its name to a response header on the way in.
func headerMiddleware(name string) middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("X-Chain", name)
			next.ServeHTTP(w, r)
		})
	}
}

func TestRouteRegistration(t *testing.T) {
	server := (&HTTPServerConfig{Addr: "127.0.0.1", Port: 9000, Log: discardLogger()}).New()
	respond := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body + ":" + r.PathValue("id")))
		}
	}
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	}

	server.Handle("GET /handle/{id}", respond("handle"))
	server.HandleFunc("GET /func/{id}", respond("func"))
	server.HandleWithMiddleware("GET /handle-mw/{id}", respond("handle-mw"), headerMiddleware("outer"), headerMiddleware("inner"))
	server.HandleFuncWithMiddleware("GET /func-mw/{id}", respond("func-mw"), headerMiddleware("outer"), headerMiddleware("inner"))
	server.HandleFuncWithMiddleware("GET /denied", respond("denied"), deny)
	server.HandleFuncWithMiddleware("GET /bare", respond("bare"))

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantBody   string
		wantChain  string
	}{
		{"Handle", http.MethodGet, "/handle/1", http.StatusOK, "handle:1", ""},
		{"HandleFunc", http.MethodGet, "/func/2", http.StatusOK, "func:2", ""},
		{"HandleWithMiddleware applies middlewares in order", http.MethodGet, "/handle-mw/3", http.StatusOK, "handle-mw:3", "outer,inner"},
		{"HandleFuncWithMiddleware applies middlewares in order", http.MethodGet, "/func-mw/4", http.StatusOK, "func-mw:4", "outer,inner"},
		{"a middleware can block the handler", http.MethodGet, "/denied", http.StatusUnauthorized, "", ""},
		{"no middlewares behaves like HandleFunc", http.MethodGet, "/bare", http.StatusOK, "bare:", ""},
		{"method mismatch is rejected by the mux", http.MethodPost, "/func/2", http.StatusMethodNotAllowed, "", ""},
		{"unknown path", http.MethodGet, "/nope", http.StatusNotFound, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			// Requests go through the handler of the real http.Server.
			server.httpServer.Handler.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantBody != "" && rec.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", rec.Body.String(), tt.wantBody)
			}
			if got := strings.Join(rec.Header().Values("X-Chain"), ","); got != tt.wantChain {
				t.Errorf("middleware chain = %q, want %q", got, tt.wantChain)
			}
		})
	}
}
