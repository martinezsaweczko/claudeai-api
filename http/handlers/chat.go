package handlers

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/martinezsaweczko/claudeai-api/http/middleware"
	"github.com/martinezsaweczko/claudeai-api/services/claude"
)

// maxChatBodyBytes caps the request body of POST /v1/chat/completions.
const maxChatBodyBytes = 8 << 20 // 8 MiB

// statusClientClosedRequest is the nginx-style status used for requests the
// client abandoned. No body is written; the code only feeds logs and metrics.
const statusClientClosedRequest = 499

// ChatHandlerConfig holds dependencies for the chat completions handler.
type ChatHandlerConfig struct {
	Log          *slog.Logger
	Backend      ChatBackend
	DefaultModel string // used when the request names no model
	BasePath     string // e.g. "/v1"
}

// ChatHandler serves the OpenAI-compatible chat completions endpoint,
// including Server-Sent Events streaming.
type ChatHandler struct {
	log          *slog.Logger
	backend      ChatBackend
	defaultModel string
	basePath     string

	newID func() string
	now   func() time.Time
}

// NewChatHandler creates the handler.
func (c *ChatHandlerConfig) NewChatHandler() *ChatHandler {
	return &ChatHandler{
		log:          c.Log,
		backend:      c.Backend,
		defaultModel: c.DefaultModel,
		basePath:     c.BasePath,
		newID:        newCompletionID,
		now:          time.Now,
	}
}

// RegisterRoutes registers POST {base}/chat/completions.
func (h *ChatHandler) RegisterRoutes(router Router, middlewares ...middleware.Middleware) {
	pattern := "POST " + h.basePath + "/chat/completions"
	if len(middlewares) > 0 {
		router.HandleFuncWithMiddleware(pattern, h.createCompletion, middlewares...)
	} else {
		router.HandleFunc(pattern, h.createCompletion)
	}
}

// chatRequest is the subset of the OpenAI request this service reads. Unknown
// fields (temperature, max_tokens, ...) are accepted and ignored.
type chatRequest struct {
	Model     string          `json:"model"`
	Messages  []chatMessage   `json:"messages"`
	Stream    bool            `json:"stream"`
	StreamOpt *streamOptions  `json:"stream_options"`
	N         *int            `json:"n"`
	Tools     json.RawMessage `json:"tools"`
	Functions json.RawMessage `json:"functions"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// chatOptions are the request settings the handler itself acts on.
type chatOptions struct {
	stream       bool
	includeUsage bool
}

type chatMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	ToolCalls    json.RawMessage `json:"tool_calls"`
	FunctionCall json.RawMessage `json:"function_call"`
}

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Documentation-only types (swagger).

type chatMessageDoc struct {
	Role    string `json:"role" example:"user" enums:"system,developer,user,assistant"`
	Content any    `json:"content" swaggertype:"string" example:"Reply with the single word OK"`
}

type chatRequestDoc struct {
	Model    string           `json:"model" example:"haiku"`
	Messages []chatMessageDoc `json:"messages"`
	Stream   bool             `json:"stream" example:"false"`
}

type chatChoice struct {
	Index        int              `json:"index" example:"0"`
	Message      chatReplyMessage `json:"message"`
	FinishReason string           `json:"finish_reason" example:"stop" enums:"stop,length"`
}

type chatReplyMessage struct {
	Role    string `json:"role" example:"assistant"`
	Content string `json:"content"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatCompletion struct {
	ID      string       `json:"id" example:"chatcmpl-0123456789abcdef01234567"`
	Object  string       `json:"object" example:"chat.completion"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   chatUsage    `json:"usage"`
}

type openAIErrorDetailDoc struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

type openAIErrorDoc struct {
	Error openAIErrorDetailDoc `json:"error"`
}

// apiError is a client-facing error: status, OpenAI type/code and a message
// that is safe to show.
type apiError struct {
	status  int
	errType string
	code    string
	message string

	retryAfter string // Retry-After header value, when set
}

func badRequest(code, message string) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: middleware.ErrorTypeInvalidRequest, code: code, message: message}
}

const toolsUnsupported = "Tool and function calling is not supported by this service."

// Create a chat completion
//
//	@Summary      Create a chat completion
//	@Description  OpenAI-compatible chat completion backed by the Claude Code CLI. With stream true the response is a text/event-stream of chat.completion.chunk events ending with data: [DONE].
//	@Description  Used fields: model (empty uses the server default), messages (string or text-part content; roles system, developer, user, assistant), stream (SSE when true), stream_options.include_usage (final usage chunk).
//	@Description  Ignored without error: temperature, top_p, max_tokens, max_completion_tokens, stop, presence_penalty, frequency_penalty, seed, user, response_format, logit_bias, metadata.
//	@Description  Rejected with 400: tools/functions/tool_calls/function_call and tool or function roles (unsupported_parameter), n > 1 (unsupported_parameter), non-text content parts (unsupported_content_type).
//	@Tags         OpenAI
//	@Accept       json
//	@Produce      json,text/event-stream
//	@Security     BearerAuth
//	@Param        request  body      chatRequestDoc  true  "Chat completion request"
//	@Success      200      {object}  chatCompletion
//	@Failure      400      {object}  openAIErrorDoc
//	@Failure      401      {object}  openAIErrorDoc
//	@Failure      404      {object}  openAIErrorDoc  "model_not_found"
//	@Failure      413      {object}  openAIErrorDoc  "request_too_large"
//	@Failure      429      {object}  openAIErrorDoc  "rate_limit_exceeded or server_busy"
//	@Failure      500      {object}  openAIErrorDoc
//	@Failure      502      {object}  openAIErrorDoc  "upstream_error or upstream_auth_error"
//	@Failure      504      {object}  openAIErrorDoc  "upstream_timeout"
//	@Router       /v1/chat/completions [post]
func (h *ChatHandler) createCompletion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	creq, opts, apiErr := h.parseRequest(w, r)
	if apiErr != nil {
		h.log.DebugContext(ctx, "Rejected chat completion request", "code", apiErr.code)
		writeAPIError(w, apiErr)
		return
	}
	if opts.stream {
		h.streamCompletion(w, r, creq, opts)
		return
	}

	res, err := h.backend.Complete(ctx, creq)
	if err != nil {
		h.fail(ctx, w, err)
		return
	}

	model := res.Model
	if model == "" {
		model = creq.Model
	}
	prompt := res.Usage.InputTokens + res.Usage.CacheCreationTokens + res.Usage.CacheReadTokens
	writeJSON(w, http.StatusOK, chatCompletion{
		ID:      h.newID(),
		Object:  "chat.completion",
		Created: h.now().Unix(),
		Model:   model,
		Choices: []chatChoice{{
			Index:        0,
			Message:      chatReplyMessage{Role: "assistant", Content: res.Text},
			FinishReason: finishReason(res.StopReason),
		}},
		Usage: chatUsage{
			PromptTokens:     prompt,
			CompletionTokens: res.Usage.OutputTokens,
			TotalTokens:      prompt + res.Usage.OutputTokens,
		},
	})
}

func finishReason(stop string) string {
	if stop == "max_tokens" {
		return "length"
	}
	return "stop"
}

// fail maps a backend error to an OpenAI error response. Only fixed messages
// reach the client; the underlying error text is logged (it is already
// sanitized by the backend) and never contains prompts or keys.
func (h *ChatHandler) fail(ctx context.Context, w http.ResponseWriter, err error) {
	if ctx.Err() != nil {
		h.log.DebugContext(ctx, "Client went away before the completion finished", "error", err)
		w.WriteHeader(statusClientClosedRequest)
		return
	}
	e := h.mapBackendError(ctx, err)
	if e.retryAfter != "" {
		w.Header().Set("Retry-After", e.retryAfter)
	}
	writeAPIError(w, e)
}

// mapBackendError turns a backend error into the client-facing error and logs
// the failure. It is shared by the JSON and the SSE error paths.
func (h *ChatHandler) mapBackendError(ctx context.Context, err error) *apiError {
	var e *apiError
	level := slog.LevelError
	switch {
	case errors.Is(err, claude.ErrInvalidRequest):
		e = badRequest("invalid_request", "The messages are not valid: the last message must be a non-empty user message.")
		level = slog.LevelInfo
	case errors.Is(err, claude.ErrInvalidModel):
		e = &apiError{status: http.StatusNotFound, errType: middleware.ErrorTypeInvalidRequest, code: "model_not_found", message: "The requested model does not exist."}
		level = slog.LevelInfo
	case errors.Is(err, claude.ErrAuth):
		e = &apiError{status: http.StatusBadGateway, errType: middleware.ErrorTypeAPI, code: "upstream_auth_error", message: "The service could not authenticate with Claude. The operator must renew the Claude login."}
	case errors.Is(err, claude.ErrRateLimited):
		e = &apiError{status: http.StatusTooManyRequests, errType: middleware.ErrorTypeRateLimit, code: "rate_limit_exceeded", message: "The Claude usage limit was reached. Try again later."}
		level = slog.LevelWarn
	case errors.Is(err, claude.ErrBusy):
		e = &apiError{status: http.StatusTooManyRequests, errType: middleware.ErrorTypeRateLimit, code: "server_busy", message: "The server is busy. Retry shortly.", retryAfter: "1"}
		level = slog.LevelWarn
	case errors.Is(err, claude.ErrTimeout):
		e = &apiError{status: http.StatusGatewayTimeout, errType: middleware.ErrorTypeAPI, code: "upstream_timeout", message: "Claude did not answer in time."}
		level = slog.LevelWarn
	case errors.Is(err, claude.ErrCLI), errors.Is(err, claude.ErrNotFound):
		e = &apiError{status: http.StatusBadGateway, errType: middleware.ErrorTypeAPI, code: "upstream_error", message: "The upstream Claude service failed."}
	default:
		e = &apiError{status: http.StatusInternalServerError, errType: middleware.ErrorTypeAPI, code: "upstream_error", message: "The upstream Claude service failed."}
	}
	h.log.Log(ctx, level, "Chat completion failed", "status", e.status, "code", e.code, "error", err)
	return e
}

func writeAPIError(w http.ResponseWriter, e *apiError) {
	middleware.WriteOpenAIError(w, e.status, e.message, e.errType, e.code)
}

// decodeBody reads exactly one JSON value from the (size-limited) body.
func decodeBody(w http.ResponseWriter, r *http.Request, req *chatRequest) *apiError {
	body := http.MaxBytesReader(w, r.Body, maxChatBodyBytes)
	dec := json.NewDecoder(body)
	err := dec.Decode(req)
	if err == nil {
		// Anything but whitespace after the first value is malformed input.
		var extra json.RawMessage
		if err = dec.Decode(&extra); errors.Is(err, io.EOF) {
			return nil
		} else if err == nil {
			return badRequest("invalid_json", "The request body must contain a single JSON object.")
		}
	}
	var tooLarge *http.MaxBytesError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &tooLarge):
		return &apiError{status: http.StatusRequestEntityTooLarge, errType: middleware.ErrorTypeInvalidRequest, code: "request_too_large", message: "The request body is too large (limit 8 MiB)."}
	case errors.As(err, &typeErr):
		return badRequest("invalid_request_body", "A request field has the wrong type: "+typeErr.Field)
	}
	return badRequest("invalid_json", "The request body is not valid JSON.")
}

// parseRequest decodes and validates the body and builds the backend request.
func (h *ChatHandler) parseRequest(w http.ResponseWriter, r *http.Request) (claude.Request, chatOptions, *apiError) {
	var req chatRequest
	if apiErr := decodeBody(w, r, &req); apiErr != nil {
		return claude.Request{}, chatOptions{}, apiErr
	}
	opts := chatOptions{stream: req.Stream, includeUsage: req.StreamOpt != nil && req.StreamOpt.IncludeUsage}

	if req.N != nil && *req.N > 1 {
		return claude.Request{}, chatOptions{}, badRequest("unsupported_parameter", "Only n = 1 is supported.")
	}
	if present(req.Tools) || present(req.Functions) {
		return claude.Request{}, chatOptions{}, badRequest("unsupported_parameter", toolsUnsupported)
	}
	if len(req.Messages) == 0 {
		return claude.Request{}, chatOptions{}, badRequest("invalid_messages", "'messages' is required and must not be empty.")
	}

	out := claude.Request{Model: strings.TrimSpace(req.Model)}
	if out.Model == "" {
		out.Model = h.defaultModel
	}
	var system []string
	for _, m := range req.Messages {
		if present(m.ToolCalls) || present(m.FunctionCall) {
			return claude.Request{}, chatOptions{}, badRequest("unsupported_parameter", toolsUnsupported)
		}
		switch m.Role {
		case "tool", "function":
			return claude.Request{}, chatOptions{}, badRequest("unsupported_parameter", toolsUnsupported)
		case "system", "developer", claude.RoleUser, claude.RoleAssistant:
		default:
			return claude.Request{}, chatOptions{}, badRequest("invalid_messages", "Unsupported message role; use system, developer, user or assistant.")
		}
		text, apiErr := contentText(m.Content)
		if apiErr != nil {
			return claude.Request{}, chatOptions{}, apiErr
		}
		if m.Role == "system" || m.Role == "developer" {
			if strings.TrimSpace(text) != "" {
				system = append(system, text)
			}
			continue
		}
		out.Messages = append(out.Messages, claude.Message{Role: m.Role, Content: text})
	}
	out.System = strings.Join(system, "\n\n")
	return out, opts, nil
}

// present reports whether an optional JSON field carries a non-empty value.
func present(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && !bytes.Equal(t, []byte("null")) && !bytes.Equal(t, []byte("[]"))
}

// contentText flattens message content: a string, or an array of text parts
// joined with newlines. Any other part type is rejected.
func contentText(raw json.RawMessage) (string, *apiError) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return "", nil
	}
	switch t[0] {
	case '"':
		var s string
		if err := json.Unmarshal(t, &s); err != nil {
			return "", badRequest("invalid_messages", "Message content is not valid.")
		}
		return s, nil
	case '[':
		var parts []contentPart
		if err := json.Unmarshal(t, &parts); err != nil {
			return "", badRequest("invalid_messages", "Message content parts are not valid.")
		}
		texts := make([]string, 0, len(parts))
		for _, p := range parts {
			if p.Type != "text" {
				return "", badRequest("unsupported_content_type", "Only text content parts are supported.")
			}
			texts = append(texts, p.Text)
		}
		return strings.Join(texts, "\n"), nil
	}
	return "", badRequest("invalid_messages", "Message content must be a string or an array of text parts.")
}

var fallbackIDCounter atomic.Uint64

func newCompletionID() string { return completionIDFrom(rand.Reader) }

// completionIDFrom builds a completion id from random bytes. If the entropy
// source fails it falls back to a time- and counter-based suffix, which stays
// unique within the process and is good enough for a non-secret identifier.
func completionIDFrom(r io.Reader) string {
	var b [12]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return fmt.Sprintf("chatcmpl-%016x%08x", time.Now().UnixNano(), uint32(fallbackIDCounter.Add(1)))
	}
	return "chatcmpl-" + hex.EncodeToString(b[:])
}
