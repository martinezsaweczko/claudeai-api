package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/martinezsaweczko/claudeai-api/http/middleware"
	"github.com/martinezsaweczko/claudeai-api/services/claude"
)

// SSE chunk types (OpenAI chat.completion.chunk).

type chunkDelta struct {
	Role    string  `json:"role,omitempty"`
	Content *string `json:"content,omitempty"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *chatUsage    `json:"usage,omitempty"`
}

// streamCompletion serves a stream:true request as Server-Sent Events.
//
// The response is committed lazily: nothing is written until the backend
// produces its first delta (or finishes), so failures before that point are
// ordinary JSON errors with the same status mapping as the non-streaming path.
// After the headers are out an error becomes one SSE error event followed by
// [DONE]. The chunks carry the requested (or default) model because the first
// chunk is sent before the CLI reports the model it resolved.
//
// There is no keep-alive heartbeat: a proxy with a short idle timeout may cut a
// stream while Claude is silent and needs a larger read timeout.
func (h *ChatHandler) streamCompletion(w http.ResponseWriter, r *http.Request, creq claude.Request, opts chatOptions) {
	ctx := r.Context()

	// Probe flush support up front: Flush itself would commit the headers.
	if !canFlush(w) {
		h.log.ErrorContext(ctx, "Streaming requested but the response writer cannot flush")
		writeAPIError(w, &apiError{status: http.StatusInternalServerError, errType: middleware.ErrorTypeAPI, code: "internal_error", message: "Streaming is not available on this server."})
		return
	}

	s := &sseStream{
		h:     h,
		w:     w,
		rc:    http.NewResponseController(w),
		id:    h.newID(),
		model: creq.Model,
		now:   h.now().Unix(),
	}

	res, err := h.backend.Stream(ctx, creq, func(e claude.Event) error {
		if e.Type != claude.EventTextDelta || e.Text == "" {
			return nil
		}
		return s.delta(ctx, e.Text)
	})

	switch {
	case err == nil:
		s.finish(ctx, res, opts)
	case ctx.Err() != nil || s.gone:
		h.log.DebugContext(ctx, "Client went away during the stream", "error", err)
		if !s.started {
			w.WriteHeader(statusClientClosedRequest)
		}
	case !s.started:
		h.fail(ctx, w, err)
	default:
		e := h.mapBackendError(ctx, err)
		s.write(openAIErrorDoc{Error: openAIErrorDetailDoc{Message: e.message, Type: e.errType, Code: e.code}})
		s.done()
	}
}

// canFlush reports whether w, or a writer it wraps, supports flushing.
func canFlush(w http.ResponseWriter) bool {
	for {
		switch v := w.(type) {
		case http.Flusher:
			return true
		case interface{ FlushError() error }:
			return true
		case interface{ Unwrap() http.ResponseWriter }:
			w = v.Unwrap()
		default:
			return false
		}
	}
}

// sseStream writes the events of one streaming response.
type sseStream struct {
	h     *ChatHandler
	w     http.ResponseWriter
	rc    *http.ResponseController
	id    string
	model string
	now   int64

	started bool // headers and role chunk are out
	gone    bool // a write failed or the client went away
}

func ptr[T any](v T) *T { return &v }

func (s *sseStream) chunk(choices []chunkChoice) chatChunk {
	return chatChunk{ID: s.id, Object: "chat.completion.chunk", Created: s.now, Model: s.model, Choices: choices}
}

// start commits the response: headers, then the role chunk.
func (s *sseStream) start(ctx context.Context) error {
	// A configured server write timeout must not cut a long stream.
	if err := s.rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		s.h.log.DebugContext(ctx, "Could not clear the write deadline", "error", err)
	}
	hd := s.w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache")
	hd.Set("Connection", "keep-alive")
	hd.Set("X-Accel-Buffering", "no")
	s.started = true
	return s.write(s.chunk([]chunkChoice{{Delta: chunkDelta{Role: "assistant", Content: ptr("")}}}))
}

// delta sends one text delta, committing the response on the first call. It
// fails as soon as the client is gone so the backend stops the CLI process.
func (s *sseStream) delta(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		s.gone = true
		return err
	}
	if !s.started {
		if err := s.start(ctx); err != nil {
			return err
		}
	}
	return s.write(s.chunk([]chunkChoice{{Delta: chunkDelta{Content: &text}}}))
}

// finish closes a successful stream.
func (s *sseStream) finish(ctx context.Context, res claude.Result, opts chatOptions) {
	if !s.started {
		// The backend produced no deltas: send the whole answer, if any.
		if res.Text != "" {
			if s.delta(ctx, res.Text) != nil {
				return
			}
		} else if s.start(ctx) != nil {
			return
		}
	}
	if s.write(s.chunk([]chunkChoice{{Delta: chunkDelta{}, FinishReason: ptr(finishReason(res.StopReason))}})) != nil {
		return
	}
	if opts.includeUsage {
		prompt := res.Usage.InputTokens + res.Usage.CacheCreationTokens + res.Usage.CacheReadTokens
		c := s.chunk([]chunkChoice{})
		c.Usage = &chatUsage{PromptTokens: prompt, CompletionTokens: res.Usage.OutputTokens, TotalTokens: prompt + res.Usage.OutputTokens}
		if s.write(c) != nil {
			return
		}
	}
	s.done()
}

func (s *sseStream) done() {
	s.writeRaw([]byte("data: [DONE]\n\n"))
}

// write sends v as one SSE data event and flushes it.
func (s *sseStream) write(v any) error {
	var buf bytes.Buffer
	buf.WriteString("data: ")
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil { // Encode appends "\n"
		return err
	}
	buf.WriteString("\n")
	return s.writeRaw(buf.Bytes())
}

func (s *sseStream) writeRaw(b []byte) error {
	if _, err := s.w.Write(b); err != nil {
		s.gone = true
		return err
	}
	if err := s.rc.Flush(); err != nil {
		s.gone = true
		return err
	}
	return nil
}
