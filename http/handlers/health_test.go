package handlers

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/martinezsaweczko/whatsappBot-golang/o11"
	"go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The health endpoint is hit by every liveness/readiness probe, so it must not
// add a span of its own nor log above Debug.
func TestHealthHandlerIsQuiet(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	obs := &o11.ObservabilityInst{Trace: tp, MeterProvider: noop.NewMeterProvider()}
	h, err := (&HealthHandlerConfig{Log: log, Observability: obs, BasePath: "/api/v1"}).NewHealthHandler()
	if err != nil {
		t.Fatalf("NewHealthHandler failed: %v", err)
	}
	router := &muxRouter{mux: http.NewServeMux()}
	h.RegisterRoutes(router)

	rec := httptest.NewRecorder()
	router.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if spans := recorder.Ended(); len(spans) != 0 {
		t.Errorf("health request created %d span(s), want none (first: %q)", len(spans), spans[0].Name())
	}
	if logs.Len() != 0 {
		t.Errorf("health request logged at Info or above: %q", logs.String())
	}
}
