package o11

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// captureHandler records the last handled slog record
type captureHandler struct {
	lastAttrs map[string]string
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.lastAttrs = map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		h.lastAttrs[a.Key] = a.Value.String()
		return true
	})
	return nil
}

func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler      { return h }

func TestTraceLogHandlerInjectsTraceIDs(t *testing.T) {
	inner := &captureHandler{}
	handler := NewTraceLogHandler(inner)
	log := slog.New(handler)

	// Without span: no trace_id
	log.InfoContext(context.Background(), "no span")
	if _, ok := inner.lastAttrs["trace_id"]; ok {
		t.Error("trace_id should not be present without a span")
	}

	// With span: trace_id and span_id injected
	traceID, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	spanID, _ := trace.SpanIDFromHex("0102030405060708")
	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanCtx)

	log.InfoContext(ctx, "with span")
	if inner.lastAttrs["trace_id"] != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("unexpected trace_id: %s", inner.lastAttrs["trace_id"])
	}
	if inner.lastAttrs["span_id"] != "0102030405060708" {
		t.Errorf("unexpected span_id: %s", inner.lastAttrs["span_id"])
	}
}

func TestTraceLogHandlerWithAttrsChain(t *testing.T) {
	inner := &captureHandler{}
	handler := NewTraceLogHandler(inner)
	log := slog.New(handler).With("app", "test").WithGroup("grp")
	log.Info("chained", "key", "value")

	if !strings.Contains(inner.lastAttrs["key"], "value") {
		t.Errorf("WithAttrs/WithGroup chain broken: %+v", inner.lastAttrs)
	}
}
