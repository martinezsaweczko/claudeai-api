package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/martinezsaweczko/claudeai-api/o11"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// tagMiddleware records when it runs, before and after the next handler.
func tagMiddleware(name string, calls *[]string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*calls = append(*calls, name+":in")
			next.ServeHTTP(w, r)
			*calls = append(*calls, name+":out")
		})
	}
}

func TestChainRunsTheFirstMiddlewareOutermost(t *testing.T) {
	want := []string{"first:in", "second:in", "third:in", "handler", "third:out", "second:out", "first:out"}

	build := map[string]func(calls *[]string) http.Handler{
		"Chain": func(calls *[]string) http.Handler {
			handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { *calls = append(*calls, "handler") })
			return Chain(handler, tagMiddleware("first", calls), tagMiddleware("second", calls), tagMiddleware("third", calls))
		},
		"HandlerFunc": func(calls *[]string) http.Handler {
			handler := func(http.ResponseWriter, *http.Request) { *calls = append(*calls, "handler") }
			return HandlerFunc(handler, tagMiddleware("first", calls), tagMiddleware("second", calls), tagMiddleware("third", calls))
		},
	}

	for name, newHandler := range build {
		t.Run(name, func(t *testing.T) {
			var calls []string
			newHandler(&calls).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

			if strings.Join(calls, " ") != strings.Join(want, " ") {
				t.Errorf("call order = %v, want %v", calls, want)
			}
		})
	}
}

func TestChainWithoutMiddlewaresReturnsTheHandler(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })

	rec := httptest.NewRecorder()
	Chain(handler).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
}

func TestChainMiddlewareCanShortCircuit(t *testing.T) {
	reached := false
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	}
	var calls []string

	rec := httptest.NewRecorder()
	Chain(handler, deny, tagMiddleware("inner", &calls)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if reached || len(calls) != 0 {
		t.Errorf("nothing after the outer middleware should run (handler=%v, inner=%v)", reached, calls)
	}
}

func TestLoggingMiddlewareLogsStatusAndSize(t *testing.T) {
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantStatus int
		wantSize   int
	}{
		{
			name: "explicit status and two writes",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte("not "))
				_, _ = w.Write([]byte("found"))
			},
			wantStatus: http.StatusNotFound,
			wantSize:   9,
		},
		{
			name:       "implicit 200 when only the body is written",
			handler:    func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hello")) },
			wantStatus: http.StatusOK,
			wantSize:   5,
		},
		{
			name:       "handler that writes nothing",
			handler:    func(http.ResponseWriter, *http.Request) {},
			wantStatus: http.StatusOK,
			wantSize:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&buf, nil))
			handler := LoggingMiddleware(log)(tt.handler)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/file?x=1", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			// The wrapper must not alter what the client receives.
			if rec.Code != tt.wantStatus || rec.Body.Len() != tt.wantSize {
				t.Errorf("client got status %d and %d bytes, want %d and %d", rec.Code, rec.Body.Len(), tt.wantStatus, tt.wantSize)
			}

			var entry map[string]any
			if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
				t.Fatalf("expected exactly one JSON log line, got %q: %v", buf.String(), err)
			}
			if entry["msg"] != "HTTP request" || entry["method"] != http.MethodPost || entry["path"] != "/api/v1/file?x=1" {
				t.Errorf("unexpected log entry: %v", entry)
			}
			if got := int(entry["status_code"].(float64)); got != tt.wantStatus {
				t.Errorf("logged status_code = %d, want %d", got, tt.wantStatus)
			}
			if got := int(entry["response_size"].(float64)); got != tt.wantSize {
				t.Errorf("logged response_size = %d, want %d", got, tt.wantSize)
			}
			if entry["remote_addr"] != req.RemoteAddr {
				t.Errorf("logged remote_addr = %v, want %s", entry["remote_addr"], req.RemoteAddr)
			}
		})
	}
}

// attrValue returns the string form of an attribute in a set ("" if absent).
func attrValue(set attribute.Set, key string) string {
	v, _ := set.Value(attribute.Key(key))
	return v.Emit()
}

func TestMetricsMiddlewareRecordsREDMetrics(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(ctx) })
	httpMetrics, err := o11.NewHTTPMetrics(mp)
	if err != nil {
		t.Fatalf("NewHTTPMetrics failed: %v", err)
	}

	collect := func(t *testing.T) map[string]metricdata.Metrics {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(ctx, &rm); err != nil {
			t.Fatalf("collect failed: %v", err)
		}
		out := map[string]metricdata.Metrics{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				out[m.Name] = m
			}
		}
		return out
	}

	inFlightDuringRequest := int64(-1)
	mux := http.NewServeMux()
	mux.Handle("GET /items/{id}", MetricsMiddleware(httpMetrics)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		inFlight := collect(t)["http_server_requests_in_flight"].Data.(metricdata.Sum[int64])
		inFlightDuringRequest = inFlight.DataPoints[0].Value
		w.WriteHeader(http.StatusTeapot)
	})))

	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/items/42", nil))
	// A request that never went through a mux has no route pattern.
	MetricsMiddleware(httpMetrics)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/direct", nil))

	if inFlightDuringRequest != 1 {
		t.Errorf("in-flight gauge during the request = %d, want 1", inFlightDuringRequest)
	}

	metrics := collect(t)

	inFlight := metrics["http_server_requests_in_flight"].Data.(metricdata.Sum[int64])
	if len(inFlight.DataPoints) != 1 || inFlight.DataPoints[0].Value != 0 {
		t.Errorf("in-flight gauge after the requests = %+v, want a single 0", inFlight.DataPoints)
	}

	type labels struct{ route, method, status string }
	requests := map[labels]int64{}
	for _, dp := range metrics["http_server_requests_total"].Data.(metricdata.Sum[int64]).DataPoints {
		requests[labels{attrValue(dp.Attributes, "route"), attrValue(dp.Attributes, "method"), attrValue(dp.Attributes, "status")}] = dp.Value
	}
	wantRequests := map[labels]int64{
		// The route label is the pattern, not the concrete path, to keep cardinality bounded.
		{"GET /items/{id}", "GET", "418"}: 1,
		{"unknown", "POST", "200"}:        1,
	}
	if len(requests) != len(wantRequests) {
		t.Errorf("request counter series = %v, want %v", requests, wantRequests)
	}
	for l, want := range wantRequests {
		if requests[l] != want {
			t.Errorf("requests_total%+v = %d, want %d", l, requests[l], want)
		}
	}

	durations := metrics["http_server_request_duration_seconds"].Data.(metricdata.Histogram[float64]).DataPoints
	if len(durations) != 2 {
		t.Fatalf("duration histogram has %d series, want 2", len(durations))
	}
	for _, dp := range durations {
		if dp.Count != 1 {
			t.Errorf("duration series %v has %d observations, want 1", dp.Attributes.ToSlice(), dp.Count)
		}
		if _, hasStatus := dp.Attributes.Value("status"); hasStatus {
			t.Errorf("duration series should not be split by status: %v", dp.Attributes.ToSlice())
		}
	}
}

func TestTracingMiddlewareNamesSpansAfterTheRoute(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	// otelhttp resolves the global tracer provider when the handler is built.
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})

	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux := http.NewServeMux()
	mux.Handle("GET /items/{id}", TracingMiddleware()(ok))
	mux.Handle("/any/{id}", TracingMiddleware()(ok))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/items/42", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want the wrapped handler's %d", rec.Code, http.StatusNoContent)
	}
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/any/9", nil))
	// Without a mux there is no pattern, so the path is used instead.
	TracingMiddleware()(ok).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodDelete, "/direct/7", nil))

	spans := recorder.Ended()
	if len(spans) != 3 {
		t.Fatalf("recorded %d spans, want one per request", len(spans))
	}
	// Routed spans are named "{method} {route}" (OTel semconv): the method
	// appears once even when the pattern carries it, and the route is the
	// pattern, never the concrete path, so span names stay low-cardinality.
	for i, want := range []string{"GET /items/{id}", "PUT /any/{id}", "DELETE /direct/7"} {
		if got := spans[i].Name(); got != want {
			t.Errorf("span %d name = %q, want %q", i, got, want)
		}
	}
}

func TestWrappedResponseWriterSupportsStreaming(t *testing.T) {
	t.Run("Flush reaches the underlying flusher", func(t *testing.T) {
		rec := httptest.NewRecorder()
		wrapped := &wrappedResponseWriter{ResponseWriter: rec, statusCode: http.StatusOK}

		if err := http.NewResponseController(wrapped).Flush(); err != nil {
			t.Fatalf("Flush through the response controller = %v, want nil", err)
		}
		if !rec.Flushed {
			t.Error("the underlying recorder was not flushed")
		}

		flusher, ok := any(wrapped).(http.Flusher)
		if !ok {
			t.Fatal("wrapped writer does not implement http.Flusher")
		}
		rec.Flushed = false
		flusher.Flush()
		if !rec.Flushed {
			t.Error("Flush did not delegate to the underlying writer")
		}
	})

	t.Run("Unwrap returns the underlying writer", func(t *testing.T) {
		rec := httptest.NewRecorder()
		wrapped := &wrappedResponseWriter{ResponseWriter: rec}

		u, ok := any(wrapped).(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			t.Fatal("wrapped writer does not implement Unwrap() http.ResponseWriter")
		}
		if u.Unwrap() != http.ResponseWriter(rec) {
			t.Error("Unwrap did not return the underlying writer")
		}
	})

	t.Run("Flush without a flusher is a no-op", func(t *testing.T) {
		wrapped := &wrappedResponseWriter{ResponseWriter: noFlushWriter{httptest.NewRecorder()}}
		wrapped.Flush() // must not panic
	})
}

// noFlushWriter hides the Flush method of the embedded recorder.
type noFlushWriter struct{ rw http.ResponseWriter }

func (w noFlushWriter) Header() http.Header         { return w.rw.Header() }
func (w noFlushWriter) Write(b []byte) (int, error) { return w.rw.Write(b) }
func (w noFlushWriter) WriteHeader(code int)        { w.rw.WriteHeader(code) }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
