package repository

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/martinezsaweczko/whatsappBot-golang/o11"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// telemetryProbe gives access to what an InstrumentedDB recorded.
type telemetryProbe struct {
	reader *sdkmetric.ManualReader
	spans  *tracetest.SpanRecorder
}

// newObservedDB wraps a temporary SQLite repository with SDK telemetry that is
// kept in memory, so the recorded metrics and spans can be asserted.
func newObservedDB(t *testing.T) (*InstrumentedDB, *telemetryProbe) {
	t.Helper()
	probe := &telemetryProbe{reader: sdkmetric.NewManualReader(), spans: tracetest.NewSpanRecorder()}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(probe.reader))
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(probe.spans))
	t.Cleanup(func() {
		_ = mp.Shutdown(context.Background())
		_ = tp.Shutdown(context.Background())
	})

	metrics, err := o11.NewRepoMetrics(mp)
	if err != nil {
		t.Fatalf("NewRepoMetrics failed: %v", err)
	}
	return NewInstrumented(newTestDB(t), metrics, tp), probe
}

// queryCounts returns repo_queries_total as "operation/result" -> count.
func (p *telemetryProbe) queryCounts(t *testing.T) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, m := range p.collect(t) {
		if m.Name != "repo_queries_total" {
			continue
		}
		for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
			op, _ := dp.Attributes.Value(attribute.Key("operation"))
			result, _ := dp.Attributes.Value(attribute.Key("result"))
			out[op.AsString()+"/"+result.AsString()] = dp.Value
		}
	}
	return out
}

// durationCounts returns repo_query_duration_seconds as operation -> observations.
func (p *telemetryProbe) durationCounts(t *testing.T) map[string]uint64 {
	t.Helper()
	out := map[string]uint64{}
	for _, m := range p.collect(t) {
		if m.Name != "repo_query_duration_seconds" {
			continue
		}
		for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
			op, _ := dp.Attributes.Value(attribute.Key("operation"))
			out[op.AsString()] = dp.Count
		}
	}
	return out
}

func (p *telemetryProbe) collect(t *testing.T) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := p.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect failed: %v", err)
	}
	var out []metricdata.Metrics
	for _, sm := range rm.ScopeMetrics {
		out = append(out, sm.Metrics...)
	}
	return out
}

// The wrappers must return exactly what the plain repository returns, and
// account for every call under its own operation name.
func TestInstrumentedSubscriptionsTokensAndUsage(t *testing.T) {
	db, probe := newObservedDB(t)
	ctx := context.Background()

	// Subscriptions.
	if err := db.SaveSubscription(ctx, "marca", "34600111222@s.whatsapp.net"); err != nil {
		t.Fatalf("SaveSubscription failed: %v", err)
	}
	if err := db.SaveSubscription(ctx, "pais", "34600333444@s.whatsapp.net"); err != nil {
		t.Fatalf("SaveSubscription failed: %v", err)
	}
	subs, err := db.ReturnSubscriptions(ctx, []string{"34600111222@s.whatsapp.net"})
	if err != nil {
		t.Fatalf("ReturnSubscriptions failed: %v", err)
	}
	if len(subs) != 1 || subs[0].SubscriptionText != "marca" || subs[0].ID == uuid.Nil {
		t.Errorf("subscriptions = %+v, want the single 'marca' row", subs)
	}
	users, err := db.MatchSubscriptions(ctx, "Diario MARCA 2026-10-12.pdf")
	if err != nil {
		t.Fatalf("MatchSubscriptions failed: %v", err)
	}
	if !slices.Equal(users, []string{"34600111222@s.whatsapp.net"}) {
		t.Errorf("matched users = %v, want only the 'marca' subscriber", users)
	}
	if err := db.DeleteSubscription(ctx, []string{"34600111222@s.whatsapp.net"}); err != nil {
		t.Fatalf("DeleteSubscription failed: %v", err)
	}
	if users, err := db.MatchSubscriptions(ctx, "Diario MARCA 2026-10-12.pdf"); err != nil || len(users) != 0 {
		t.Errorf("after delete matched %v (err %v), want nobody", users, err)
	}

	// JWT replay protection.
	if count, err := db.CountTokenUses(ctx, "token-a"); err != nil || count != 0 {
		t.Errorf("unused token count = %d (err %v), want 0", count, err)
	}
	for range 2 {
		if err := db.SaveToken(ctx, "token-a"); err != nil {
			t.Fatalf("SaveToken failed: %v", err)
		}
	}
	if count, err := db.CountTokenUses(ctx, "token-a"); err != nil || count != 2 {
		t.Errorf("token count = %d (err %v), want 2", count, err)
	}
	if err := db.CleanJWT(ctx); err != nil {
		t.Fatalf("CleanJWT failed: %v", err)
	}
	// Fresh tokens are inside the retention window and survive the cleanup.
	if count, err := db.CountTokenUses(ctx, "token-a"); err != nil || count != 2 {
		t.Errorf("token count after cleanup = %d (err %v), want 2", count, err)
	}

	// Usage statistics.
	if err := db.ReportFileUsage(ctx, "a.pdf", 200); err != nil {
		t.Fatalf("ReportFileUsage failed: %v", err)
	}
	if err := db.ReportUserUsage(ctx, "David", "a.pdf"); err != nil {
		t.Fatalf("ReportUserUsage failed: %v", err)
	}
	var fileResult int
	if err := db.db.QueryRow("SELECT result FROM file_usage WHERE file = ?", "a.pdf").Scan(&fileResult); err != nil || fileResult != 200 {
		t.Errorf("stored file usage result = %d (err %v), want 200", fileResult, err)
	}
	var usageUser string
	if err := db.db.QueryRow("SELECT user FROM user_usage WHERE file = ?", "a.pdf").Scan(&usageUser); err != nil || usageUser != "David" {
		t.Errorf("stored usage user = %q (err %v), want David", usageUser, err)
	}

	wantCalls := map[string]int64{
		"subscriptions.insert/ok": 2,
		"subscriptions.select/ok": 1,
		"subscriptions.match/ok":  2,
		"subscriptions.delete/ok": 1,
		"jwt_used.count/ok":       3,
		"jwt_used.insert/ok":      2,
		"jwt_used.clean/ok":       1,
		"file_usage.insert/ok":    1,
		"user_usage.insert/ok":    1,
	}
	gotCalls := probe.queryCounts(t)
	if len(gotCalls) != len(wantCalls) {
		t.Errorf("repo_queries_total series = %v, want %v", gotCalls, wantCalls)
	}
	for series, want := range wantCalls {
		if gotCalls[series] != want {
			t.Errorf("repo_queries_total{%s} = %d, want %d", series, gotCalls[series], want)
		}
	}

	durations := probe.durationCounts(t)
	for series, want := range wantCalls {
		operation := strings.TrimSuffix(series, "/ok")
		if durations[operation] != uint64(want) {
			t.Errorf("repo_query_duration_seconds{%s} has %d observations, want %d", operation, durations[operation], want)
		}
	}

	// One span per call, named after the operation, none marked as failed.
	spans := probe.spans.Ended()
	if len(spans) != 14 {
		t.Errorf("recorded %d spans, want 14 (one per repository call)", len(spans))
	}
	for _, span := range spans {
		if !strings.HasPrefix(span.Name(), "repository.") {
			t.Errorf("span name %q should start with repository.", span.Name())
		}
		if span.Status().Code == codes.Error {
			t.Errorf("span %q is marked as failed", span.Name())
		}
	}
}

// A failing query must be returned to the caller unchanged, counted with
// result=error and recorded on its span.
func TestInstrumentedRecordsFailures(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		operation string
		call      func(db *InstrumentedDB) error
	}{
		{"subscriptions.insert", func(db *InstrumentedDB) error { return db.SaveSubscription(ctx, "marca", "user") }},
		{"subscriptions.delete", func(db *InstrumentedDB) error { return db.DeleteSubscription(ctx, []string{"user"}) }},
		{"subscriptions.select", func(db *InstrumentedDB) error {
			subs, err := db.ReturnSubscriptions(ctx, []string{"user"})
			if subs != nil {
				t.Errorf("ReturnSubscriptions returned %v alongside an error", subs)
			}
			return err
		}},
		{"subscriptions.match", func(db *InstrumentedDB) error {
			users, err := db.MatchSubscriptions(ctx, "a.pdf")
			if users != nil {
				t.Errorf("MatchSubscriptions returned %v alongside an error", users)
			}
			return err
		}},
		{"jwt_used.count", func(db *InstrumentedDB) error {
			count, err := db.CountTokenUses(ctx, "token")
			if count != 0 {
				t.Errorf("CountTokenUses returned %d alongside an error", count)
			}
			return err
		}},
		{"jwt_used.insert", func(db *InstrumentedDB) error { return db.SaveToken(ctx, "token") }},
		{"jwt_used.clean", func(db *InstrumentedDB) error { return db.CleanJWT(ctx) }},
		{"file_usage.insert", func(db *InstrumentedDB) error { return db.ReportFileUsage(ctx, "a.pdf", 404) }},
		{"user_usage.insert", func(db *InstrumentedDB) error { return db.ReportUserUsage(ctx, "David", "a.pdf") }},
	}

	for _, tt := range tests {
		t.Run(tt.operation, func(t *testing.T) {
			db, probe := newObservedDB(t)
			// Every query fails once the connection pool is closed.
			if err := db.Close(); err != nil {
				t.Fatalf("failed to close the database: %v", err)
			}

			err := tt.call(db)
			if err == nil {
				t.Fatal("expected an error from a closed database")
			}
			if !strings.Contains(err.Error(), "database is closed") {
				t.Errorf("error = %v, want the underlying driver error to be preserved", err)
			}

			calls := probe.queryCounts(t)
			if len(calls) != 1 || calls[tt.operation+"/error"] != 1 {
				t.Errorf("repo_queries_total = %v, want only %s/error = 1", calls, tt.operation)
			}
			if got := probe.durationCounts(t)[tt.operation]; got != 1 {
				t.Errorf("failed call has %d duration observations, want 1", got)
			}

			spans := probe.spans.Ended()
			if len(spans) != 1 {
				t.Fatalf("recorded %d spans, want 1", len(spans))
			}
			span := spans[0]
			if span.Name() != "repository."+tt.operation {
				t.Errorf("span name = %q, want repository.%s", span.Name(), tt.operation)
			}
			if span.Status().Code != codes.Error || span.Status().Description != err.Error() {
				t.Errorf("span status = %+v, want an error carrying %q", span.Status(), err)
			}
			if len(span.Events()) != 1 || span.Events()[0].Name != "exception" {
				t.Errorf("span events = %+v, want the recorded error", span.Events())
			}
		})
	}
}

// Empty identity sets short-circuit in the repository but are still counted as
// a successful operation by the decorator.
func TestInstrumentedEmptyIdentitySets(t *testing.T) {
	db, probe := newObservedDB(t)
	ctx := context.Background()

	if err := db.SaveSubscription(ctx, "marca", "user"); err != nil {
		t.Fatalf("SaveSubscription failed: %v", err)
	}
	if err := db.DeleteSubscription(ctx, nil); err != nil {
		t.Fatalf("DeleteSubscription(nil) failed: %v", err)
	}
	subs, err := db.ReturnSubscriptions(ctx, nil)
	if err != nil || subs != nil {
		t.Errorf("ReturnSubscriptions(nil) = %v, %v; want no rows and no error", subs, err)
	}
	// Deleting an empty set must not delete everything.
	if users, err := db.MatchSubscriptions(ctx, "marca.pdf"); err != nil || len(users) != 1 {
		t.Errorf("subscription should survive an empty delete, matched %v (err %v)", users, err)
	}

	calls := probe.queryCounts(t)
	if calls["subscriptions.delete/ok"] != 1 || calls["subscriptions.select/ok"] != 1 {
		t.Errorf("empty-set calls not counted as ok: %v", calls)
	}
}
