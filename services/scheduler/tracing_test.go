package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/martinezsaweczko/whatsappBot-golang/model"
	"go.mau.fi/whatsmeow/types"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// tracedService returns a scheduler wired to an in-memory span recorder.
func tracedService(t *testing.T, store *fakeStore, checker *fakeChecker) (*Service, *tracetest.SpanRecorder) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	exec := &fakeExecutor{commands: []string{"electricidad"}}
	svc, err := New(store, exec, checker, testLogger(), tp, Config{Timezone: "Europe/Madrid"})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	return svc, recorder
}

// singleSpan returns the only ended span with the given name.
func singleSpan(t *testing.T, recorder *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, s := range recorder.Ended() {
		if s.Name() == name {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d %q span(s), want exactly 1", len(found), name)
	}
	return found[0]
}

func hasEvent(span sdktrace.ReadOnlySpan, name string) bool {
	for _, e := range span.Events() {
		if e.Name == name {
			return true
		}
	}
	return false
}

func TestOperationsProduceSpans(t *testing.T) {
	storeErr := errors.New("database is down")
	connected := &fakeChecker{connected: true, ownJID: types.NewJID("bot", types.DefaultUserServer)}

	tests := []struct {
		name       string
		store      *fakeStore
		run        func(svc *Service) error
		span       string
		wantErr    bool
		wantStatus codes.Code
	}{
		{
			name:  "List",
			store: &fakeStore{},
			run: func(svc *Service) error {
				_, err := svc.List(context.Background())
				return err
			},
			span:       "scheduler.List",
			wantStatus: codes.Unset,
		},
		{
			name:  "List store failure",
			store: &fakeStore{listErr: storeErr},
			run: func(svc *Service) error {
				_, err := svc.List(context.Background())
				return err
			},
			span:       "scheduler.List",
			wantErr:    true,
			wantStatus: codes.Error,
		},
		{
			name:  "Delete",
			store: &fakeStore{cmds: []model.ScheduledCommand{{ID: uuid.MustParse("0198a7c2-0000-7000-8000-000000000001")}}},
			run: func(svc *Service) error {
				return svc.Delete(context.Background(), uuid.MustParse("0198a7c2-0000-7000-8000-000000000001"))
			},
			span:       "scheduler.Delete",
			wantStatus: codes.Unset,
		},
		{
			name:  "Delete unknown ID",
			store: &fakeStore{},
			run: func(svc *Service) error {
				return svc.Delete(context.Background(), uuid.MustParse("0198a7c2-0000-7000-8000-000000000001"))
			},
			span:       "scheduler.Delete",
			wantErr:    true,
			wantStatus: codes.Error,
		},
		{
			name:  "Start",
			store: &fakeStore{},
			run: func(svc *Service) error {
				err := svc.Start(context.Background())
				svc.Stop()
				return err
			},
			span:       "scheduler.Start",
			wantStatus: codes.Unset,
		},
		{
			name:  "Start store failure",
			store: &fakeStore{listErr: storeErr},
			run: func(svc *Service) error {
				return svc.Start(context.Background())
			},
			span:       "scheduler.Start",
			wantErr:    true,
			wantStatus: codes.Error,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, recorder := tracedService(t, tt.store, connected)

			err := tt.run(svc)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}

			span := singleSpan(t, recorder, tt.span)
			if got := span.Status().Code; got != tt.wantStatus {
				t.Errorf("span status = %v, want %v", got, tt.wantStatus)
			}
			if got := hasEvent(span, "exception"); got != tt.wantErr {
				t.Errorf("span has an exception event = %v, want %v", got, tt.wantErr)
			}
		})
	}
}

// A job skipped because WhatsApp is down is expected behaviour and must not
// show up as a failed span; a bad JID or a failing command must.
func TestRunJobMarksOutcomeOnSpan(t *testing.T) {
	tests := []struct {
		name        string
		connected   bool
		command     string
		groupJID    string
		wantStatus  codes.Code
		wantSkipped bool
		wantError   bool
	}{
		{name: "executed", connected: true, command: "electricidad", groupJID: "123456789@g.us", wantStatus: codes.Unset},
		{name: "skipped while disconnected", command: "electricidad", groupJID: "123456789@g.us", wantStatus: codes.Unset, wantSkipped: true},
		{name: "invalid group JID", connected: true, command: "electricidad", groupJID: "1.x:y@s.whatsapp.net", wantStatus: codes.Error, wantError: true},
		{name: "command fails", connected: true, command: "unknown", groupJID: "123456789@g.us", wantStatus: codes.Error, wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker := &fakeChecker{connected: tt.connected, ownJID: types.NewJID("bot", types.DefaultUserServer)}
			svc, recorder := tracedService(t, &fakeStore{}, checker)

			svc.runJob(uuid.Must(uuid.NewV7()), "0 8 * * *", tt.command, tt.groupJID)

			span := singleSpan(t, recorder, "scheduler.RunJob")
			if got := span.Status().Code; got != tt.wantStatus {
				t.Errorf("span status = %v, want %v", got, tt.wantStatus)
			}
			if got := hasEvent(span, "skipped"); got != tt.wantSkipped {
				t.Errorf("span has a skipped event = %v, want %v", got, tt.wantSkipped)
			}
			if got := hasEvent(span, "exception"); got != tt.wantError {
				t.Errorf("span has an exception event = %v, want %v", got, tt.wantError)
			}
		})
	}
}
