package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/martinezsaweczko/whatsappBot-golang/config"
	"github.com/martinezsaweczko/whatsappBot-golang/http/middleware"
	"github.com/martinezsaweczko/whatsappBot-golang/model"
	"github.com/martinezsaweczko/whatsappBot-golang/o11"
	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
	"go.mau.fi/whatsmeow/types"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestObservability returns telemetry backed by the SDK with a manual
// reader, so handler counters can be inspected without any exporter.
func newTestObservability(t *testing.T) (*o11.ObservabilityInst, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() {
		_ = mp.Shutdown(context.Background())
		_ = tp.Shutdown(context.Background())
	})
	return &o11.ObservabilityInst{Trace: tp, MeterProvider: mp}, reader
}

// counterPoints collects once and returns the data points of an int64 counter.
func counterPoints(t *testing.T, reader *sdkmetric.ManualReader, name string) []metricdata.DataPoint[int64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect failed: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m.Data.(metricdata.Sum[int64]).DataPoints
			}
		}
	}
	return nil
}

func pointAttr(dp metricdata.DataPoint[int64], key string) string {
	v, _ := dp.Attributes.Value(attribute.Key(key))
	return v.Emit()
}

func TestHealthHandler(t *testing.T) {
	obs, reader := newTestObservability(t)
	h, err := (&HealthHandlerConfig{Log: quietLogger(), Observability: obs, BasePath: "/api/v1"}).NewHealthHandler()
	if err != nil {
		t.Fatalf("NewHealthHandler failed: %v", err)
	}
	router := &muxRouter{mux: http.NewServeMux()}
	h.RegisterRoutes(router)

	before := time.Now().UTC().Truncate(time.Second)
	rec := httptest.NewRecorder()
	router.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q, want application/json", ct)
	}
	var info model.HealthInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("invalid JSON body %q: %v", rec.Body.String(), err)
	}
	if info.Status != "ok" {
		t.Errorf("status field = %q, want ok", info.Status)
	}
	ts, err := time.Parse(time.RFC3339, info.Timestamp)
	if err != nil {
		t.Fatalf("timestamp %q is not RFC 3339: %v", info.Timestamp, err)
	}
	if ts.Before(before) || ts.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("timestamp %v is not the current time", ts)
	}

	points := counterPoints(t, reader, "health_handler_calls")
	if len(points) != 1 || points[0].Value != 1 {
		t.Fatalf("health_handler_calls points = %+v, want a single call", points)
	}
	if got := pointAttr(points[0], "http_code"); got != "200" {
		t.Errorf("http_code label = %q, want 200", got)
	}
	if got := pointAttr(points[0], "path"); got != "/api/v1/health" {
		t.Errorf("path label = %q, want /api/v1/health", got)
	}

	// Only GET is routed.
	rec = httptest.NewRecorder()
	router.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/health", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rec.Code)
	}
}

func TestVersionHandler(t *testing.T) {
	obs, reader := newTestObservability(t)
	build := model.BuildInfo{Version: "v1.2.3", BuildTime: "2026-10-12T08:00:00Z", AppName: "whatsappbot-golang"}
	h, err := (&VersionHandlerConfig{Log: quietLogger(), Observability: obs, AppBuildInfo: build, BasePath: "/api/v1"}).NewVersionHandler()
	if err != nil {
		t.Fatalf("NewVersionHandler failed: %v", err)
	}
	router := &muxRouter{mux: http.NewServeMux()}
	h.RegisterRoutes(router)

	rec := httptest.NewRecorder()
	router.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q, want application/json", ct)
	}
	// The wire format uses camelCase keys, unlike model.BuildInfo.
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON body %q: %v", rec.Body.String(), err)
	}
	want := map[string]string{"version": "v1.2.3", "buildTime": "2026-10-12T08:00:00Z", "appName": "whatsappbot-golang"}
	if len(body) != len(want) {
		t.Errorf("body = %v, want %v", body, want)
	}
	for key, value := range want {
		if body[key] != value {
			t.Errorf("body[%q] = %q, want %q", key, body[key], value)
		}
	}

	points := counterPoints(t, reader, "version_handler_calls")
	if len(points) != 1 || points[0].Value != 1 || pointAttr(points[0], "http_code") != "200" {
		t.Errorf("version_handler_calls points = %+v, want a single 200 call", points)
	}
}

func TestMetricsHandlerRegisterRoutes(t *testing.T) {
	exposition := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# HELP up 1"))
	})
	passthrough := func(next http.Handler) http.Handler { return next }

	tests := []struct {
		name            string
		prometheusPath  string
		withMiddleware  bool
		requestPath     string
		wantStatus      int
		wantMiddlewares int
	}{
		{"serves the handler on the configured path", "/metrics", false, "/metrics", http.StatusOK, 0},
		{"custom path", "/internal/prom", false, "/internal/prom", http.StatusOK, 0},
		{"default path is not registered when a custom one is set", "/internal/prom", false, "/metrics", http.StatusNotFound, 0},
		{"middlewares are applied", "/metrics", true, "/metrics", http.StatusOK, 1},
		{"an empty path disables the endpoint", "", false, "/metrics", http.StatusNotFound, 0},
		{"an empty path disables the endpoint with middlewares too", "", true, "/metrics", http.StatusNotFound, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{O11: config.O11Config{PrometheusPath: tt.prometheusPath}}
			h := (&MetricsHandlerConfig{Log: quietLogger(), Cfg: cfg}).NewMetricsHandler()
			router := &muxRouter{mux: http.NewServeMux()}
			if tt.withMiddleware {
				h.RegisterRoutes(router, exposition, passthrough)
			} else {
				h.RegisterRoutes(router, exposition)
			}

			rec := httptest.NewRecorder()
			router.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.requestPath, nil))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusOK && rec.Body.String() != "# HELP up 1" {
				t.Errorf("body = %q, want the wrapped handler's output", rec.Body.String())
			}
			if router.withMiddlewares != tt.wantMiddlewares {
				t.Errorf("routes registered with middlewares = %d, want %d", router.withMiddlewares, tt.wantMiddlewares)
			}
		})
	}
}

// stubSender is a whatsapp.Sender that does nothing; the trigger handler only
// passes it through to the notifier.
type stubSender struct{}

func (stubSender) ReplyText(context.Context, whatsapp.IncomingMessage, string) error { return nil }
func (stubSender) SendText(context.Context, types.JID, string) error                 { return nil }
func (stubSender) ReplyMedia(context.Context, whatsapp.IncomingMessage, []byte, string, string) error {
	return nil
}
func (stubSender) SendMedia(context.Context, types.JID, []byte, string, string) error { return nil }
func (stubSender) DeleteMessage(context.Context, whatsapp.IncomingMessage) error      { return nil }

// notifyCall is one invocation of fakeNotifier.Notify.
type notifyCall struct {
	sender      whatsapp.Sender
	category    string
	file        string
	hasDeadline bool
}

// fakeNotifier reports each Notify call on a channel, because the handler
// calls it from a background goroutine.
type fakeNotifier struct {
	calls chan notifyCall
	err   error
}

func (f *fakeNotifier) Notify(ctx context.Context, sender whatsapp.Sender, category, filename string) error {
	_, hasDeadline := ctx.Deadline()
	f.calls <- notifyCall{sender: sender, category: category, file: filename, hasDeadline: hasDeadline}
	return f.err
}

func newTriggerRouter(t *testing.T, notifier *fakeNotifier, sender whatsapp.Sender) *muxRouter {
	t.Helper()
	h := (&SubscriptionTriggerHandlerConfig{
		Log:      quietLogger(),
		TP:       tracenoop.NewTracerProvider(),
		Notifier: notifier,
		Sender:   sender,
		BasePath: "/api/v1",
	}).NewSubscriptionTriggerHandler()
	router := &muxRouter{mux: http.NewServeMux()}
	h.RegisterRoutes(router)
	return router
}

func TestSubscriptionTriggerNotifiesInTheBackground(t *testing.T) {
	tests := []struct {
		name      string
		notifyErr error
	}{
		{"notification succeeds", nil},
		// The request has already been answered, so a failure is only logged.
		{"notification fails", errors.New("whatsapp is down")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			notifier := &fakeNotifier{calls: make(chan notifyCall, 1), err: tt.notifyErr}
			sender := stubSender{}
			router := newTriggerRouter(t, notifier, sender)

			// A canceled request context must not cancel the notification.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/file",
				strings.NewReader(`{"category":"nacional","file":"El Pais 2026-10-12.pdf"}`)).WithContext(ctx)
			rec := httptest.NewRecorder()
			router.mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			var resp statusResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Status != "Sent" {
				t.Errorf("body = %q, want status Sent (err %v)", rec.Body.String(), err)
			}

			select {
			case call := <-notifier.calls:
				if call.category != "nacional" || call.file != "El Pais 2026-10-12.pdf" {
					t.Errorf("notified %q/%q, want nacional/El Pais 2026-10-12.pdf", call.category, call.file)
				}
				if call.sender != whatsapp.Sender(sender) {
					t.Error("the configured sender was not passed to the notifier")
				}
				if !call.hasDeadline {
					t.Error("the background notification should run with its own timeout")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the notifier was never called")
			}
		})
	}
}

func TestSubscriptionTriggerValidation(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus string
	}{
		{"malformed JSON", `{"category":`, "Invalid body"},
		{"empty body", ``, "Invalid body"},
		{"missing file", `{"category":"nacional"}`, "category and file are required"},
		{"missing category", `{"file":"a.pdf"}`, "category and file are required"},
		{"empty object", `{}`, "category and file are required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			notifier := &fakeNotifier{calls: make(chan notifyCall, 1)}
			router := newTriggerRouter(t, notifier, stubSender{})

			rec := httptest.NewRecorder()
			router.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/file", strings.NewReader(tt.body)))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			var resp statusResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Status != tt.wantStatus {
				t.Errorf("body = %q, want status %q (err %v)", rec.Body.String(), tt.wantStatus, err)
			}
			// The handler returns before starting the background notification.
			if len(notifier.calls) != 0 {
				t.Error("an invalid request must not notify anyone")
			}
		})
	}
}

// failingListService is a scheduler service whose List always fails.
type failingListService struct {
	fakeSchedulerService
}

func (failingListService) List(context.Context) ([]model.ScheduledCommand, error) {
	return nil, errors.New("database is locked")
}

func TestSchedulerListStoreFailure(t *testing.T) {
	h, err := SchedulerHandlerConfig{Log: quietLogger(), Service: &failingListService{}, BasePath: "/api/v1"}.NewSchedulerHandler()
	if err != nil {
		t.Fatalf("NewSchedulerHandler failed: %v", err)
	}

	rec := httptest.NewRecorder()
	h.list(rec, httptest.NewRequest(http.MethodGet, "/api/v1/scheduler", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var resp errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON body %q: %v", rec.Body.String(), err)
	}
	if resp.Error != "failed to list scheduled commands" {
		t.Errorf("error = %q", resp.Error)
	}
	// The cause stays in the logs; it is not leaked to the client.
	if strings.Contains(rec.Body.String(), "database is locked") {
		t.Errorf("the store error leaked into the response: %s", rec.Body.String())
	}
}

func TestSchedulerListEmptyIsAnArray(t *testing.T) {
	h, _ := newSchedulerHandler(t)

	rec := httptest.NewRecorder()
	h.list(rec, httptest.NewRequest(http.MethodGet, "/api/v1/scheduler", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("body = %q, want an empty JSON array rather than null", got)
	}
}

func TestHandlerConstructorsRequireDependencies(t *testing.T) {
	tests := []struct {
		name    string
		build   func() error
		wantErr string
	}{
		{"notification without a logger", func() error {
			_, err := NotificationHandlerConfig{Sender: &fakeNotificationSender{}}.NewNotificationHandler()
			return err
		}, "logger is required"},
		{"notification without a sender", func() error {
			_, err := NotificationHandlerConfig{Log: quietLogger()}.NewNotificationHandler()
			return err
		}, "sender is required"},
		{"scheduler without a logger", func() error {
			_, err := SchedulerHandlerConfig{Service: &fakeSchedulerService{}}.NewSchedulerHandler()
			return err
		}, "logger is required"},
		{"scheduler without a service", func() error {
			_, err := SchedulerHandlerConfig{Log: quietLogger()}.NewSchedulerHandler()
			return err
		}, "service is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.build()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// Every handler registers its routes through the plain methods without
// middlewares and through the *WithMiddleware ones otherwise. Requests go
// through a real ServeMux, so the method and path patterns are exercised too.
func TestRegisterRoutesWithAndWithoutMiddlewares(t *testing.T) {
	obs, _ := newTestObservability(t)
	passthrough := middleware.Middleware(func(next http.Handler) http.Handler { return next })
	group := types.NewJID("120363420531996267", types.GroupServer)

	health, err := (&HealthHandlerConfig{Log: quietLogger(), Observability: obs, BasePath: "/api/v1"}).NewHealthHandler()
	if err != nil {
		t.Fatalf("NewHealthHandler failed: %v", err)
	}
	version, err := (&VersionHandlerConfig{Log: quietLogger(), Observability: obs, BasePath: "/api/v1"}).NewVersionHandler()
	if err != nil {
		t.Fatalf("NewVersionHandler failed: %v", err)
	}
	notification, err := NotificationHandlerConfig{Log: quietLogger(), Sender: &fakeNotificationSender{}, BasePath: "/api/v1"}.NewNotificationHandler()
	if err != nil {
		t.Fatalf("NewNotificationHandler failed: %v", err)
	}
	groups := newWhatsAppHandler(t, &fakeGroupClient{})
	trigger := (&SubscriptionTriggerHandlerConfig{
		Log:      quietLogger(),
		TP:       tracenoop.NewTracerProvider(),
		Notifier: &fakeNotifier{calls: make(chan notifyCall, 8)},
		Sender:   stubSender{},
		BasePath: "/api/v1",
	}).NewSubscriptionTriggerHandler()

	type request struct {
		method string
		path   string
		body   string
		want   int
	}
	tests := []struct {
		name     string
		register func(Router, ...middleware.Middleware)
		routes   int
		requests []request
	}{
		{
			name:     "health",
			register: func(r Router, mw ...middleware.Middleware) { health.RegisterRoutes(r, mw...) },
			routes:   1,
			requests: []request{{http.MethodGet, "/api/v1/health", "", http.StatusOK}},
		},
		{
			name:     "version",
			register: func(r Router, mw ...middleware.Middleware) { version.RegisterRoutes(r, mw...) },
			routes:   1,
			requests: []request{{http.MethodGet, "/api/v1/version", "", http.StatusOK}},
		},
		{
			name: "notifications",
			register: func(r Router, mw ...middleware.Middleware) {
				notification.RegisterRoutes(r, mw...)
			},
			routes: 1,
			requests: []request{
				{http.MethodPost, "/api/v1/notifications", `{"recipient":"15551234567","message":"hello"}`, http.StatusAccepted},
				{http.MethodGet, "/api/v1/notifications", "", http.StatusMethodNotAllowed},
			},
		},
		{
			name:     "whatsapp groups",
			register: func(r Router, mw ...middleware.Middleware) { groups.RegisterRoutes(r, mw...) },
			routes:   2,
			requests: []request{
				{http.MethodGet, "/api/v1/whatsapp/groups", "", http.StatusOK},
				// The {jid} wildcard reaches the handler, which validates it.
				{http.MethodGet, "/api/v1/whatsapp/groups/" + group.String() + "/participants", "", http.StatusOK},
				{http.MethodPost, "/api/v1/whatsapp/groups", "", http.StatusMethodNotAllowed},
			},
		},
		{
			name:     "subscription trigger",
			register: func(r Router, mw ...middleware.Middleware) { trigger.RegisterRoutes(r, mw...) },
			routes:   1,
			requests: []request{
				{http.MethodPost, "/api/v1/file", `{"category":"nacional","file":"a.pdf"}`, http.StatusOK},
				{http.MethodGet, "/api/v1/file", "", http.StatusMethodNotAllowed},
			},
		},
	}

	for _, tt := range tests {
		for _, withMiddleware := range []bool{false, true} {
			name := tt.name + " without middlewares"
			if withMiddleware {
				name = tt.name + " with middlewares"
			}
			t.Run(name, func(t *testing.T) {
				router := &muxRouter{mux: http.NewServeMux()}
				wantWithMiddlewares := 0
				if withMiddleware {
					tt.register(router, passthrough)
					wantWithMiddlewares = tt.routes
				} else {
					tt.register(router)
				}

				if router.withMiddlewares != wantWithMiddlewares {
					t.Errorf("routes registered with middlewares = %d, want %d", router.withMiddlewares, wantWithMiddlewares)
				}
				for _, req := range tt.requests {
					rec := httptest.NewRecorder()
					router.mux.ServeHTTP(rec, httptest.NewRequest(req.method, req.path, strings.NewReader(req.body)))
					if rec.Code != req.want {
						t.Errorf("%s %s = %d, want %d: %s", req.method, req.path, rec.Code, req.want, rec.Body.String())
					}
				}
			})
		}
	}
}
