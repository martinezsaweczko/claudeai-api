package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/martinezsaweczko/claudeai-api/services/claude"
)

type ctxKey struct{}

type fakeChatBackend struct {
	res   claude.Result
	err   error
	calls int
	req   claude.Request
	ctx   context.Context

	// stream, when set, implements Stream.
	stream func(ctx context.Context, req claude.Request, emit func(claude.Event) error) (claude.Result, error)
}

func (f *fakeChatBackend) Stream(ctx context.Context, req claude.Request, emit func(claude.Event) error) (claude.Result, error) {
	f.calls++
	f.req = req
	f.ctx = ctx
	if f.stream != nil {
		return f.stream(ctx, req, emit)
	}
	return f.res, f.err
}

func (f *fakeChatBackend) Complete(ctx context.Context, req claude.Request) (claude.Result, error) {
	f.calls++
	f.req = req
	f.ctx = ctx
	return f.res, f.err
}

func newChatTestHandler(t *testing.T, be ChatBackend, logs io.Writer) *muxRouter {
	t.Helper()
	if logs == nil {
		logs = io.Discard
	}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := (&ChatHandlerConfig{Log: log, Backend: be, DefaultModel: "sonnet", BasePath: "/v1"}).NewChatHandler()
	h.newID = func() string { return "chatcmpl-test" }
	h.now = func() time.Time { return time.Unix(1700000000, 0) }
	router := &muxRouter{mux: http.NewServeMux()}
	h.RegisterRoutes(router)
	return router
}

func postChat(router *muxRouter, body string) *httptest.ResponseRecorder {
	return postChatReq(router, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
}

func postChatReq(router *muxRouter, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	router.mux.ServeHTTP(rec, req)
	return rec
}

type errBody struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    string  `json:"code"`
	} `json:"error"`
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) errBody {
	t.Helper()
	var e errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body is not JSON: %v: %q", err, rec.Body.String())
	}
	return e
}

func TestChatCompletionSuccess(t *testing.T) {
	tests := []struct {
		name       string
		res        claude.Result
		wantFinish string
		wantModel  string
		wantPrompt int
		wantTotal  int
	}{
		{
			name:       "end_turn with cache tokens",
			res:        claude.Result{Text: "OK", Model: "claude-haiku-4-5", StopReason: "end_turn", Usage: claude.Usage{InputTokens: 10, CacheCreationTokens: 20, CacheReadTokens: 30, OutputTokens: 5}},
			wantFinish: "stop", wantModel: "claude-haiku-4-5", wantPrompt: 60, wantTotal: 65,
		},
		{
			name:       "max_tokens maps to length",
			res:        claude.Result{Text: "cut", Model: "claude-haiku-4-5", StopReason: "max_tokens", Usage: claude.Usage{InputTokens: 1, OutputTokens: 2}},
			wantFinish: "length", wantModel: "claude-haiku-4-5", wantPrompt: 1, wantTotal: 3,
		},
		{
			name:       "empty stop reason and missing model fall back",
			res:        claude.Result{Text: "x"},
			wantFinish: "stop", wantModel: "haiku",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			be := &fakeChatBackend{res: tc.res}
			router := newChatTestHandler(t, be, nil)
			rec := postChat(router, `{"model":"haiku","messages":[{"role":"user","content":"hi"}]}`)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q", ct)
			}
			var got struct {
				ID      string `json:"id"`
				Object  string `json:"object"`
				Created int64  `json:"created"`
				Model   string `json:"model"`
				Choices []struct {
					Index   int `json:"index"`
					Message struct {
						Role    string `json:"role"`
						Content string `json:"content"`
					} `json:"message"`
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
				Usage struct {
					Prompt     int `json:"prompt_tokens"`
					Completion int `json:"completion_tokens"`
					Total      int `json:"total_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.ID != "chatcmpl-test" || got.Object != "chat.completion" || got.Created != 1700000000 || got.Model != tc.wantModel {
				t.Errorf("envelope = %+v", got)
			}
			if len(got.Choices) != 1 || got.Choices[0].Index != 0 || got.Choices[0].Message.Role != "assistant" ||
				got.Choices[0].Message.Content != tc.res.Text || got.Choices[0].FinishReason != tc.wantFinish {
				t.Errorf("choices = %+v", got.Choices)
			}
			if got.Usage.Prompt != tc.wantPrompt || got.Usage.Completion != tc.res.Usage.OutputTokens || got.Usage.Total != tc.wantTotal {
				t.Errorf("usage = %+v", got.Usage)
			}
		})
	}
}

func TestChatCompletionRequestMapping(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantModel  string
		wantSystem string
		wantMsgs   []claude.Message
	}{
		{
			name:      "default model when empty",
			body:      `{"messages":[{"role":"user","content":"hi"}]}`,
			wantModel: "sonnet",
			wantMsgs:  []claude.Message{{Role: "user", Content: "hi"}},
		},
		{
			name:      "content parts are joined with newline",
			body:      `{"model":"opus","messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`,
			wantModel: "opus",
			wantMsgs:  []claude.Message{{Role: "user", Content: "a\nb"}},
		},
		{
			name:       "system and developer merge in order",
			body:       `{"messages":[{"role":"system","content":"S1"},{"role":"developer","content":[{"type":"text","text":"D1"}]},{"role":"user","content":"q"},{"role":"assistant","content":"a"},{"role":"user","content":"q2"}]}`,
			wantModel:  "sonnet",
			wantSystem: "S1\n\nD1",
			wantMsgs:   []claude.Message{{Role: "user", Content: "q"}, {Role: "assistant", Content: "a"}, {Role: "user", Content: "q2"}},
		},
		{
			name:      "trailing whitespace after the body is fine",
			body:      "{\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}\n \t\r\n",
			wantModel: "sonnet",
			wantMsgs:  []claude.Message{{Role: "user", Content: "hi"}},
		},
		{
			name:      "ignored fields and unknown fields are accepted",
			body:      `{"messages":[{"role":"user","content":"hi"}],"temperature":0.2,"top_p":1,"max_tokens":5,"max_completion_tokens":5,"stop":["x"],"presence_penalty":0,"frequency_penalty":0,"seed":1,"user":"u","stream_options":{"include_usage":true},"response_format":{"type":"text"},"logit_bias":{},"metadata":{"a":"b"},"stream":false,"n":1,"tools":[],"whatever":1}`,
			wantModel: "sonnet",
			wantMsgs:  []claude.Message{{Role: "user", Content: "hi"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			be := &fakeChatBackend{res: claude.Result{Text: "ok"}}
			rec := postChat(newChatTestHandler(t, be, nil), tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			if be.req.Model != tc.wantModel || be.req.System != tc.wantSystem {
				t.Errorf("req model/system = %q/%q, want %q/%q", be.req.Model, be.req.System, tc.wantModel, tc.wantSystem)
			}
			if fmt.Sprint(be.req.Messages) != fmt.Sprint(tc.wantMsgs) {
				t.Errorf("messages = %v, want %v", be.req.Messages, tc.wantMsgs)
			}
		})
	}
}

func TestChatCompletionRejectsBadRequests(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantType   string
		wantCode   string
	}{
		{"bad json", `{"messages":`, 400, "invalid_request_error", "invalid_json"},
		{"wrong field type", `{"messages":"nope"}`, 400, "invalid_request_error", "invalid_request_body"},
		{"missing messages", `{}`, 400, "invalid_request_error", "invalid_messages"},
		{"empty messages", `{"messages":[]}`, 400, "invalid_request_error", "invalid_messages"},
		{"unknown role", `{"messages":[{"role":"wizard","content":"x"}]}`, 400, "invalid_request_error", "invalid_messages"},
		{"image part", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, 400, "invalid_request_error", "unsupported_content_type"},
		{"audio part", `{"messages":[{"role":"user","content":[{"type":"input_audio"}]}]}`, 400, "invalid_request_error", "unsupported_content_type"},
		{"object content", `{"messages":[{"role":"user","content":{"a":1}}]}`, 400, "invalid_request_error", "invalid_messages"},
		{"tool role", `{"messages":[{"role":"tool","content":"x"}]}`, 400, "invalid_request_error", "unsupported_parameter"},
		{"function role", `{"messages":[{"role":"function","content":"x"}]}`, 400, "invalid_request_error", "unsupported_parameter"},
		{"tool_calls", `{"messages":[{"role":"user","content":"x"},{"role":"assistant","content":null,"tool_calls":[{"id":"1"}]}]}`, 400, "invalid_request_error", "unsupported_parameter"},
		{"function_call", `{"messages":[{"role":"assistant","content":"","function_call":{"name":"f"}}]}`, 400, "invalid_request_error", "unsupported_parameter"},
		{"tools", `{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function"}]}`, 400, "invalid_request_error", "unsupported_parameter"},
		{"functions", `{"messages":[{"role":"user","content":"x"}],"functions":[{"name":"f"}]}`, 400, "invalid_request_error", "unsupported_parameter"},
		{"n greater than one", `{"messages":[{"role":"user","content":"x"}],"n":2}`, 400, "invalid_request_error", "unsupported_parameter"},
		{"trailing data", `{"messages":[{"role":"user","content":"x"}]} {"a":1}`, 400, "invalid_request_error", "invalid_json"},
		{"trailing garbage", `{"messages":[{"role":"user","content":"x"}]}xyz`, 400, "invalid_request_error", "invalid_json"},
		{"stream_options wrong type", `{"messages":[{"role":"user","content":"x"}],"stream_options":{"include_usage":"yes"}}`, 400, "invalid_request_error", "invalid_request_body"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			be := &fakeChatBackend{}
			rec := postChat(newChatTestHandler(t, be, nil), tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			e := decodeErr(t, rec)
			if e.Error.Type != tc.wantType || e.Error.Code != tc.wantCode || e.Error.Message == "" {
				t.Errorf("error = %+v", e.Error)
			}
			if be.calls != 0 {
				t.Errorf("backend called %d times for a rejected request", be.calls)
			}
		})
	}
}

func TestChatCompletionBodyTooLarge(t *testing.T) {
	be := &fakeChatBackend{}
	body := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", maxChatBodyBytes) + `"}]}`
	rec := postChat(newChatTestHandler(t, be, nil), body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if e := decodeErr(t, rec); e.Error.Code != "request_too_large" {
		t.Errorf("code = %q", e.Error.Code)
	}
	if be.calls != 0 {
		t.Error("backend must not be called")
	}
}

func TestChatCompletionBackendErrors(t *testing.T) {
	const secret = "SECRET-STDERR-/home/user/token"
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantType   string
		wantCode   string
		retryAfter string
	}{
		{"invalid request", fmt.Errorf("%w: last message must be from user", claude.ErrInvalidRequest), 400, "invalid_request_error", "invalid_request", ""},
		{"invalid model", fmt.Errorf("%w: %s", claude.ErrInvalidModel, secret), 404, "invalid_request_error", "model_not_found", ""},
		{"auth", fmt.Errorf("%w: %s", claude.ErrAuth, secret), 502, "api_error", "upstream_auth_error", ""},
		{"rate limited", fmt.Errorf("%w: %s", claude.ErrRateLimited, secret), 429, "rate_limit_error", "rate_limit_exceeded", ""},
		{"busy", fmt.Errorf("%w: context deadline", claude.ErrBusy), 429, "rate_limit_error", "server_busy", "1"},
		{"timeout", fmt.Errorf("%w after 5m", claude.ErrTimeout), 504, "api_error", "upstream_timeout", ""},
		{"cli", fmt.Errorf("%w: exit code 1: %s", claude.ErrCLI, secret), 502, "api_error", "upstream_error", ""},
		{"not found", fmt.Errorf("%w: %s", claude.ErrNotFound, secret), 502, "api_error", "upstream_error", ""},
		{"unknown", errors.New(secret), 500, "api_error", "upstream_error", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			be := &fakeChatBackend{err: tc.err}
			rec := postChat(newChatTestHandler(t, be, &logs), `{"messages":[{"role":"user","content":"PROMPT-TEXT"}]}`)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			e := decodeErr(t, rec)
			if e.Error.Type != tc.wantType || e.Error.Code != tc.wantCode {
				t.Errorf("error = %+v", e.Error)
			}
			if got := rec.Header().Get("Retry-After"); got != tc.retryAfter {
				t.Errorf("Retry-After = %q, want %q", got, tc.retryAfter)
			}
			if tc.wantStatus >= 500 || tc.wantCode == "upstream_auth_error" {
				if strings.Contains(rec.Body.String(), "SECRET") || strings.Contains(rec.Body.String(), "exit code") {
					t.Errorf("raw error leaked to the client: %s", rec.Body.String())
				}
			}
			if strings.Contains(logs.String(), "PROMPT-TEXT") {
				t.Errorf("prompt leaked into logs: %s", logs.String())
			}
			if logs.Len() == 0 {
				t.Error("backend failure was not logged")
			}
		})
	}
}

func TestChatCompletionClientCancelWritesNoBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	be := &cancelBackend{cancel: cancel}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	rec := postChatReq(newChatTestHandler(t, be, nil), req)
	if rec.Body.Len() != 0 {
		t.Errorf("body written after client cancel: %q", rec.Body.String())
	}
	if rec.Code != 499 {
		t.Errorf("status = %d, want 499", rec.Code)
	}
}

type cancelBackend struct{ cancel context.CancelFunc }

func (c *cancelBackend) Stream(ctx context.Context, req claude.Request, _ func(claude.Event) error) (claude.Result, error) {
	return c.Complete(ctx, req)
}

func (c *cancelBackend) Complete(ctx context.Context, _ claude.Request) (claude.Result, error) {
	c.cancel()
	return claude.Result{}, fmt.Errorf("%w: %w", claude.ErrBusy, ctx.Err())
}

func TestChatCompletionPropagatesRequestContext(t *testing.T) {
	be := &fakeChatBackend{res: claude.Result{Text: "ok"}}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, "marker"))
	postChatReq(newChatTestHandler(t, be, nil), req)
	if be.ctx == nil || be.ctx.Value(ctxKey{}) != "marker" {
		t.Error("request context was not passed to the backend")
	}
}

func TestChatCompletionMethodNotAllowed(t *testing.T) {
	rec := httptest.NewRecorder()
	newChatTestHandler(t, &fakeChatBackend{}, nil).mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}
