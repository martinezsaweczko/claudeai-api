package tts

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func TestSynthesize(t *testing.T) {
	audio := []byte("fake-mp3-bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "hola mundo" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("tl") != "es" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Write(audio)
	}))
	defer server.Close()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc, err := New(server.Client(), log, tracenoop.NewTracerProvider(), noop.NewMeterProvider())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// Point the service at the test server by rewriting the endpoint via transport
	svc.client = &http.Client{
		Transport: &rewriteTransport{target: server.URL, base: http.DefaultTransport},
	}

	got, err := svc.Synthesize(context.Background(), "hola mundo")
	if err != nil {
		t.Fatalf("Synthesize failed: %v", err)
	}
	if string(got) != string(audio) {
		t.Errorf("unexpected audio: %q", got)
	}
}

func TestSynthesizeEmpty(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc, _ := New(http.DefaultClient, log, tracenoop.NewTracerProvider(), noop.NewMeterProvider())
	if _, err := svc.Synthesize(context.Background(), "   "); err == nil {
		t.Fatal("expected error for empty text")
	}
}

// A failed synthesis replies to the user and returns nil from the command
// handler, so the tts.Synthesize span is the only place the failure shows up.
func TestSynthesizeSpanStatus(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		upstream   int
		wantErr    bool
		wantStatus codes.Code
	}{
		{name: "audio generated", text: "hola mundo", upstream: http.StatusOK, wantStatus: codes.Unset},
		{name: "empty text", text: "   ", upstream: http.StatusOK, wantErr: true, wantStatus: codes.Error},
		{name: "upstream failure", text: "hola mundo", upstream: http.StatusInternalServerError, wantErr: true, wantStatus: codes.Error},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.upstream)
				w.Write([]byte("fake-mp3-bytes"))
			}))
			defer server.Close()

			recorder := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
			client := &http.Client{Transport: &rewriteTransport{target: server.URL, base: http.DefaultTransport}}
			svc, err := New(client, log, tp, noop.NewMeterProvider())
			if err != nil {
				t.Fatalf("New failed: %v", err)
			}

			_, err = svc.Synthesize(context.Background(), tt.text)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Synthesize error = %v, wantErr %v", err, tt.wantErr)
			}

			spans := recorder.Ended()
			if len(spans) != 1 || spans[0].Name() != "tts.Synthesize" {
				t.Fatalf("ended spans = %d, want a single tts.Synthesize span", len(spans))
			}
			if got := spans[0].Status().Code; got != tt.wantStatus {
				t.Errorf("span status = %v, want %v", got, tt.wantStatus)
			}
			if got := len(spans[0].Events()) == 1; got != tt.wantErr {
				t.Errorf("span has a recorded error = %v, want %v", got, tt.wantErr)
			}
		})
	}
}

// rewriteTransport rewrites all requests to the test server (endpoint is a const)
type rewriteTransport struct {
	target string
	base   http.RoundTripper
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	targetURL, err := url.Parse(t.target)
	if err != nil {
		return nil, err
	}
	req.URL.Scheme = targetURL.Scheme
	req.URL.Host = targetURL.Host
	return t.base.RoundTrip(req)
}
