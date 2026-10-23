package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/martinezsaweczko/claudeai-api/services/claude"
)

const streamBody = `{"model":"haiku","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// deltaStream returns a fake Stream that emits the deltas, then the result.
func deltaStream(res claude.Result, deltas ...string) func(context.Context, claude.Request, func(claude.Event) error) (claude.Result, error) {
	return func(_ context.Context, _ claude.Request, emit func(claude.Event) error) (claude.Result, error) {
		for _, d := range deltas {
			if err := emit(claude.Event{Type: claude.EventTextDelta, Text: d}); err != nil {
				return claude.Result{}, err
			}
		}
		if err := emit(claude.Event{Type: claude.EventResult, Result: &res}); err != nil {
			return claude.Result{}, err
		}
		return res, nil
	}
}

// sseEvents splits an SSE body into the payload of each "data:" event.
func sseEvents(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, block := range strings.Split(body, "\n\n") {
		if block == "" {
			continue
		}
		payload, ok := strings.CutPrefix(block, "data: ")
		if !ok || strings.Contains(payload, "\n") {
			t.Fatalf("malformed SSE event %q in %q", block, body)
		}
		out = append(out, payload)
	}
	return out
}

func postStream(t *testing.T, be ChatBackend, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postChat(newChatTestHandler(t, be, nil), body)
}

func TestChatStreamChunks(t *testing.T) {
	const head = `{"id":"chatcmpl-test","object":"chat.completion.chunk","created":1700000000,"model":"haiku","choices":`
	role := head + `[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`
	delta := func(s string) string {
		return head + `[{"index":0,"delta":{"content":"` + s + `"},"finish_reason":null}]}`
	}
	finish := func(r string) string {
		return head + `[{"index":0,"delta":{},"finish_reason":"` + r + `"}]}`
	}

	tests := []struct {
		name   string
		body   string
		res    claude.Result
		deltas []string
		want   []string
	}{
		{
			name:   "deltas then stop without usage",
			body:   streamBody,
			res:    claude.Result{Text: "Hello", StopReason: "end_turn", Usage: claude.Usage{InputTokens: 1, OutputTokens: 2}},
			deltas: []string{"Hel", "lo"},
			want:   []string{role, delta("Hel"), delta("lo"), finish("stop"), "[DONE]"},
		},
		{
			name:   "max_tokens maps to length",
			body:   streamBody,
			res:    claude.Result{Text: "cut", StopReason: "max_tokens"},
			deltas: []string{"cut"},
			want:   []string{role, delta("cut"), finish("length"), "[DONE]"},
		},
		{
			name:   "include_usage adds a final usage chunk",
			body:   `{"model":"haiku","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`,
			res:    claude.Result{Text: "ok", StopReason: "end_turn", Usage: claude.Usage{InputTokens: 10, CacheCreationTokens: 20, CacheReadTokens: 30, OutputTokens: 5}},
			deltas: []string{"ok"},
			want: []string{role, delta("ok"), finish("stop"),
				head + `[],"usage":{"prompt_tokens":60,"completion_tokens":5,"total_tokens":65}}`, "[DONE]"},
		},
		{
			name:   "include_usage false omits usage",
			body:   `{"model":"haiku","stream":true,"stream_options":{"include_usage":false},"messages":[{"role":"user","content":"hi"}]}`,
			res:    claude.Result{Text: "ok"},
			deltas: []string{"ok"},
			want:   []string{role, delta("ok"), finish("stop"), "[DONE]"},
		},
		{
			name:   "special characters are JSON escaped",
			body:   streamBody,
			res:    claude.Result{Text: "x"},
			deltas: []string{"a\\nb"},
			want:   []string{role, delta(`a\\nb`), finish("stop"), "[DONE]"},
		},
		{
			name: "result text is sent as one delta when the backend produced none",
			body: streamBody,
			res:  claude.Result{Text: "whole"},
			want: []string{role, delta("whole"), finish("stop"), "[DONE]"},
		},
		{
			name: "empty answer still opens and closes the stream",
			body: streamBody,
			res:  claude.Result{},
			want: []string{role, finish("stop"), "[DONE]"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			be := &fakeChatBackend{stream: deltaStream(tc.res, tc.deltas...)}
			rec := postStream(t, be, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			got := sseEvents(t, rec.Body.String())
			if len(got) != len(tc.want) {
				t.Fatalf("events = %d, want %d:\n%s", len(got), len(tc.want), strings.Join(got, "\n"))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("event %d =\n%s\nwant\n%s", i, got[i], tc.want[i])
				}
			}
			if be.calls != 1 {
				t.Errorf("backend calls = %d", be.calls)
			}
		})
	}
}

func TestChatStreamHeadersAndModel(t *testing.T) {
	be := &fakeChatBackend{stream: deltaStream(claude.Result{Model: "claude-other"}, "x")}
	rec := postStream(t, be, `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	for k, v := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"Connection":        "keep-alive",
		"X-Accel-Buffering": "no",
	} {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	// The model is fixed before the backend reports its own: the default is used
	// consistently in every chunk.
	for _, ev := range sseEvents(t, rec.Body.String()) {
		if ev != "[DONE]" && !strings.Contains(ev, `"model":"sonnet"`) {
			t.Errorf("chunk without the requested/default model: %s", ev)
		}
	}
}

var typedStreamErrors = []struct {
	name       string
	err        error
	wantStatus int
	wantCode   string
}{
	{"invalid request", fmt.Errorf("%w: last message", claude.ErrInvalidRequest), 400, "invalid_request"},
	{"invalid model", fmt.Errorf("%w: SECRET", claude.ErrInvalidModel), 404, "model_not_found"},
	{"auth", fmt.Errorf("%w: SECRET", claude.ErrAuth), 502, "upstream_auth_error"},
	{"rate limited", fmt.Errorf("%w: SECRET", claude.ErrRateLimited), 429, "rate_limit_exceeded"},
	{"busy", fmt.Errorf("%w: ctx", claude.ErrBusy), 429, "server_busy"},
	{"timeout", fmt.Errorf("%w after 5m", claude.ErrTimeout), 504, "upstream_timeout"},
	{"cli", fmt.Errorf("%w: exit code 1: SECRET", claude.ErrCLI), 502, "upstream_error"},
	{"not found", fmt.Errorf("%w: SECRET", claude.ErrNotFound), 502, "upstream_error"},
	{"unknown", errors.New("SECRET exit code"), 500, "upstream_error"},
}

func TestChatStreamErrorBeforeFirstDeltaIsJSON(t *testing.T) {
	for _, tc := range typedStreamErrors {
		t.Run(tc.name, func(t *testing.T) {
			be := &fakeChatBackend{err: tc.err}
			streamRec := postStream(t, be, streamBody)
			plainRec := postStream(t, be, `{"model":"haiku","messages":[{"role":"user","content":"hi"}]}`)

			if streamRec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body %s", streamRec.Code, tc.wantStatus, streamRec.Body.String())
			}
			if ct := streamRec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if e := decodeErr(t, streamRec); e.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", e.Error.Code, tc.wantCode)
			}
			if streamRec.Body.String() != plainRec.Body.String() || streamRec.Code != plainRec.Code ||
				streamRec.Header().Get("Retry-After") != plainRec.Header().Get("Retry-After") {
				t.Errorf("streaming and non-streaming errors differ:\n%d %s\n%d %s", streamRec.Code, streamRec.Body.String(), plainRec.Code, plainRec.Body.String())
			}
			if strings.Contains(streamRec.Body.String(), "SECRET") || strings.Contains(streamRec.Body.String(), "exit code") {
				t.Errorf("raw error leaked: %s", streamRec.Body.String())
			}
		})
	}
}

func TestChatStreamErrorAfterFirstDeltaIsSSEEvent(t *testing.T) {
	for _, tc := range typedStreamErrors {
		t.Run(tc.name, func(t *testing.T) {
			be := &fakeChatBackend{stream: func(_ context.Context, _ claude.Request, emit func(claude.Event) error) (claude.Result, error) {
				if err := emit(claude.Event{Type: claude.EventTextDelta, Text: "partial"}); err != nil {
					return claude.Result{}, err
				}
				return claude.Result{}, tc.err
			}}
			rec := postStream(t, be, streamBody)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (already committed)", rec.Code)
			}
			ev := sseEvents(t, rec.Body.String())
			if len(ev) != 4 || ev[3] != "[DONE]" || !strings.Contains(ev[1], "partial") {
				t.Fatalf("events = %q", ev)
			}
			var got errBody
			if err := json.Unmarshal([]byte(ev[2]), &got); err != nil {
				t.Fatalf("error event is not JSON: %v: %s", err, ev[2])
			}
			if got.Error.Code != tc.wantCode || got.Error.Message == "" || got.Error.Type == "" {
				t.Errorf("error event = %+v", got.Error)
			}
			if !strings.Contains(ev[2], `"param":null`) {
				t.Errorf("param must be null: %s", ev[2])
			}
			if strings.Contains(rec.Body.String(), "SECRET") || strings.Contains(rec.Body.String(), "exit code") {
				t.Errorf("raw error leaked: %s", rec.Body.String())
			}
		})
	}
}

func TestChatStreamClientCancelMidStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var emitErr error
	be := &fakeChatBackend{stream: func(_ context.Context, _ claude.Request, emit func(claude.Event) error) (claude.Result, error) {
		if err := emit(claude.Event{Type: claude.EventTextDelta, Text: "first"}); err != nil {
			return claude.Result{}, err
		}
		cancel()
		emitErr = emit(claude.Event{Type: claude.EventTextDelta, Text: "second"})
		return claude.Result{}, emitErr
	}}
	var logs strings.Builder
	router := newChatTestHandler(t, be, &logs)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody)).WithContext(ctx)
	rec := postChatReq(router, req)

	if emitErr == nil {
		t.Fatal("emit must fail once the client is gone")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "first") || strings.Contains(body, "second") {
		t.Errorf("body = %q", body)
	}
	if strings.Contains(body, `"error"`) || strings.Contains(body, "[DONE]") {
		t.Errorf("nothing may be written after the disconnect: %q", body)
	}
	if !strings.Contains(logs.String(), "level=DEBUG") {
		t.Errorf("disconnect was not logged at debug: %s", logs.String())
	}
}

func TestChatStreamClientCancelBeforeFirstDelta(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	be := &fakeChatBackend{stream: func(ctx context.Context, _ claude.Request, _ func(claude.Event) error) (claude.Result, error) {
		cancel()
		return claude.Result{}, fmt.Errorf("%w: %w", claude.ErrBusy, ctx.Err())
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody)).WithContext(ctx)
	rec := postChatReq(newChatTestHandler(t, be, nil), req)
	if rec.Code != 499 || rec.Body.Len() != 0 {
		t.Errorf("got %d %q, want 499 and no body", rec.Code, rec.Body.String())
	}
}

func TestChatStreamRejectedRequestsStayJSON(t *testing.T) {
	bodies := map[string]string{
		"tools":     `{"stream":true,"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function"}]}`,
		"n":         `{"stream":true,"n":2,"messages":[{"role":"user","content":"x"}]}`,
		"image":     `{"stream":true,"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`,
		"no msgs":   `{"stream":true}`,
		"bad json":  `{"stream":true,`,
		"tool role": `{"stream":true,"messages":[{"role":"tool","content":"x"}]}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			be := &fakeChatBackend{}
			rec := postStream(t, be, body)
			if rec.Code != 400 || rec.Header().Get("Content-Type") != "application/json" {
				t.Errorf("got %d %q", rec.Code, rec.Header().Get("Content-Type"))
			}
			if be.calls != 0 {
				t.Error("backend must not be called")
			}
		})
	}
}

// noFlushWriter hides the Flusher of the wrapped writer.
type noFlushWriter struct{ w http.ResponseWriter }

func (n noFlushWriter) Header() http.Header         { return n.w.Header() }
func (n noFlushWriter) Write(b []byte) (int, error) { return n.w.Write(b) }
func (n noFlushWriter) WriteHeader(c int)           { n.w.WriteHeader(c) }

func TestChatStreamWithoutFlushSupportFailsBeforeHeaders(t *testing.T) {
	be := &fakeChatBackend{stream: deltaStream(claude.Result{}, "x")}
	router := newChatTestHandler(t, be, nil)
	rec := httptest.NewRecorder()
	router.mux.ServeHTTP(noFlushWriter{rec}, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody)))
	if rec.Code != 500 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("got %d %q: %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if be.calls != 0 {
		t.Error("backend must not run when streaming is impossible")
	}
}

// deadlineWriter records SetWriteDeadline calls.
type deadlineWriter struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

func TestChatStreamClearsWriteDeadline(t *testing.T) {
	be := &fakeChatBackend{stream: deltaStream(claude.Result{}, "x")}
	router := newChatTestHandler(t, be, nil)
	dw := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	router.mux.ServeHTTP(dw, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody)))
	if len(dw.deadlines) == 0 || !dw.deadlines[0].IsZero() {
		t.Errorf("write deadlines = %v, want the zero time first", dw.deadlines)
	}
}

func TestChatStreamIsDeliveredIncrementally(t *testing.T) {
	release := make(chan struct{})
	be := &fakeChatBackend{stream: func(_ context.Context, _ claude.Request, emit func(claude.Event) error) (claude.Result, error) {
		if err := emit(claude.Event{Type: claude.EventTextDelta, Text: "one"}); err != nil {
			return claude.Result{}, err
		}
		<-release
		if err := emit(claude.Event{Type: claude.EventTextDelta, Text: "two"}); err != nil {
			return claude.Result{}, err
		}
		return claude.Result{}, nil
	}}
	srv := httptest.NewServer(newChatTestHandler(t, be, nil).mux)
	defer srv.Close()
	// release must be closed even when an assertion fails, or Close would hang.
	var once = func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}
	defer once()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(streamBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
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
		t.Fatal("no data received before the backend was released: the stream is buffered")
	}
	once()

	var rest []string
	timeout := time.After(5 * time.Second)
loop:
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				break loop
			}
			rest = append(rest, l)
		case <-timeout:
			t.Fatal("stream did not finish")
		}
	}
	all := strings.Join(rest, "\n")
	if !strings.Contains(all, `"content":"two"`) || !strings.HasSuffix(strings.TrimSpace(all), "data: [DONE]") {
		t.Errorf("rest of the stream = %q", all)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
}

func TestNewCompletionIDFallsBackWhenRandFails(t *testing.T) {
	a := completionIDFrom(failingReader{})
	b := completionIDFrom(failingReader{})
	if !strings.HasPrefix(a, "chatcmpl-") || len(a) < len("chatcmpl-")+8 {
		t.Errorf("fallback id = %q", a)
	}
	if a == b {
		t.Errorf("fallback ids must be unique, got %q twice", a)
	}
	if id := newCompletionID(); !strings.HasPrefix(id, "chatcmpl-") || len(id) != len("chatcmpl-")+24 {
		t.Errorf("id = %q", id)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy exhausted") }
