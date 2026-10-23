package services

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/martinezsaweczko/claudeai-api/o11"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestExternalClientNamesSpansAfterMethodAndService(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	// otelhttp resolves the global tracer provider when the transport is built.
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	m, err := o11.NewServiceMetrics(noop.NewMeterProvider())
	if err != nil {
		t.Fatalf("NewServiceMetrics: %v", err)
	}
	client := NewExternalClient("openweather", 5*time.Second, m)

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req, err := http.NewRequest(method, srv.URL+"/data/2.5/weather?q=Madrid", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s request: %v", method, err)
		}
		_ = resp.Body.Close()
	}

	spans := recorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("recorded %d spans, want one per request", len(spans))
	}
	// The service name tells calls apart while keeping span names
	// low-cardinality: no host, path or query.
	for i, want := range []string{"GET openweather", "POST openweather"} {
		if got := spans[i].Name(); got != want {
			t.Errorf("span %d name = %q, want %q", i, got, want)
		}
	}
}
