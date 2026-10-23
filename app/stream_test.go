package app

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/martinezsaweczko/claudeai-api/services/claude"
)

// gatedBackend emits one delta, then blocks until released.
type gatedBackend struct{ release chan struct{} }

func (b *gatedBackend) Complete(context.Context, claude.Request) (claude.Result, error) {
	return claude.Result{}, nil
}

func (b *gatedBackend) Stream(ctx context.Context, _ claude.Request, emit func(claude.Event) error) (claude.Result, error) {
	if err := emit(claude.Event{Type: claude.EventTextDelta, Text: "one"}); err != nil {
		return claude.Result{}, err
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return claude.Result{}, ctx.Err()
	}
	if err := emit(claude.Event{Type: claude.EventTextDelta, Text: "two"}); err != nil {
		return claude.Result{}, err
	}
	return claude.Result{Text: "onetwo", Usage: claude.Usage{InputTokens: 1, OutputTokens: 2}}, nil
}

const streamChatBody = `{"model":"haiku","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`

func postStream(t *testing.T, url, auth string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(streamChatBody))
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestStreamingThroughMiddlewareChain proves that tracing, metrics, logging and
// API-key auth pass Flush through, so SSE events arrive while the backend is
// still running.
func TestStreamingThroughMiddlewareChain(t *testing.T) {
	tests := []struct {
		name string
		keys []string
		auth string
	}{
		{"no api keys", nil, ""},
		{"api key", []string{"secret"}, "Bearer secret"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			be := &gatedBackend{release: make(chan struct{})}
			release := func() {
				select {
				case <-be.release:
				default:
					close(be.release)
				}
			}
			defer release()
			public, _ := wiredServersWith(t, tc.keys, be)
			srv := httptest.NewServer(public.mux)
			defer srv.Close()

			resp := postStream(t, srv.URL, tc.auth)
			if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
				t.Fatalf("got %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
			}
			lines := make(chan string, 64)
			go func() {
				sc := bufio.NewScanner(resp.Body)
				for sc.Scan() {
					lines <- sc.Text()
				}
				close(lines)
			}()
			select {
			case l := <-lines:
				if !strings.HasPrefix(l, "data: ") {
					t.Fatalf("first line = %q", l)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no event before the backend was released: a middleware buffers the stream")
			}
			release()

			var rest []string
			deadline := time.After(5 * time.Second)
		loop:
			for {
				select {
				case l, ok := <-lines:
					if !ok {
						break loop
					}
					rest = append(rest, l)
				case <-deadline:
					t.Fatal("stream did not finish")
				}
			}
			all := strings.Join(rest, "\n")
			if !strings.Contains(all, `"content":"two"`) || !strings.Contains(all, `"total_tokens":3`) || !strings.HasSuffix(strings.TrimSpace(all), "data: [DONE]") {
				t.Errorf("rest of the stream = %q", all)
			}
		})
	}
}

func TestStreamingWithoutAPIKeyIsJSON401(t *testing.T) {
	be := &gatedBackend{release: make(chan struct{})}
	public, _ := wiredServersWith(t, []string{"secret"}, be)
	srv := httptest.NewServer(public.mux)
	defer srv.Close()

	resp := postStream(t, srv.URL, "")
	if resp.StatusCode != 401 || resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("got %d %q, want 401 application/json", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}
