package o11

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// TraceLogHandler is an slog.Handler that injects trace_id and span_id
// from the record's context, enabling log <-> trace correlation in Grafana
type TraceLogHandler struct {
	inner slog.Handler
}

// NewTraceLogHandler wraps an existing slog.Handler with trace context injection
func NewTraceLogHandler(inner slog.Handler) *TraceLogHandler {
	return &TraceLogHandler{inner: inner}
}

func (h *TraceLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *TraceLogHandler) Handle(ctx context.Context, r slog.Record) error {
	spanCtx := trace.SpanFromContext(ctx).SpanContext()
	if spanCtx.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", spanCtx.TraceID().String()),
			slog.String("span_id", spanCtx.SpanID().String()),
		)
	}
	return h.inner.Handle(ctx, r)
}

func (h *TraceLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TraceLogHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *TraceLogHandler) WithGroup(name string) slog.Handler {
	return &TraceLogHandler{inner: h.inner.WithGroup(name)}
}
