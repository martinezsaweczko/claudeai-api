package o11

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func attrSet(kv ...attribute.KeyValue) attribute.Set { return attribute.NewSet(kv...) }

// sumPoints returns value by label set for an int64 or float64 sum.
func sumPoints[N int64 | float64](t *testing.T, m metricdata.Metrics) map[attribute.Set]N {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[N])
	if !ok {
		t.Fatalf("metric %q is %T", m.Name, m.Data)
	}
	if !sum.IsMonotonic {
		t.Errorf("counter %q must be monotonic", m.Name)
	}
	out := map[attribute.Set]N{}
	for _, dp := range sum.DataPoints {
		out[dp.Attributes] = dp.Value
	}
	return out
}

func gaugePoints(t *testing.T, m metricdata.Metrics) map[attribute.Set]int64 {
	t.Helper()
	g, ok := m.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("metric %q is %T, want an int64 gauge", m.Name, m.Data)
	}
	out := map[attribute.Set]int64{}
	for _, dp := range g.DataPoints {
		out[dp.Attributes] = dp.Value
	}
	return out
}

func floatGaugePoints(t *testing.T, m metricdata.Metrics) map[attribute.Set]float64 {
	t.Helper()
	g, ok := m.Data.(metricdata.Gauge[float64])
	if !ok {
		t.Fatalf("metric %q is %T, want a float64 gauge", m.Name, m.Data)
	}
	out := map[attribute.Set]float64{}
	for _, dp := range g.DataPoints {
		out[dp.Attributes] = dp.Value
	}
	return out
}

func histPoints(t *testing.T, m metricdata.Metrics) map[attribute.Set]metricdata.HistogramDataPoint[float64] {
	t.Helper()
	h, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("metric %q is %T", m.Name, m.Data)
	}
	out := map[attribute.Set]metricdata.HistogramDataPoint[float64]{}
	for _, dp := range h.DataPoints {
		out[dp.Attributes] = dp
	}
	return out
}

func TestClaudeMetricsInstruments(t *testing.T) {
	ctx := context.Background()
	mp, reader := newManualMeterProvider(t)
	m, err := NewClaudeMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterInFlight(func() int64 { return 3 }); err != nil {
		t.Fatal(err)
	}

	sonnet := attribute.String("model", "claude-sonnet-5-5")
	m.RunsTotal.Add(ctx, 2, metric.WithAttributes(sonnet, attribute.String("outcome", "success")))
	m.RunsTotal.Add(ctx, 1, metric.WithAttributes(sonnet, attribute.String("outcome", "timeout")))
	m.RunDuration.Record(ctx, 12.5, metric.WithAttributes(sonnet))
	m.TimeToFirstToken.Record(ctx, 1.2, metric.WithAttributes(sonnet))
	m.QueueWait.Record(ctx, 0, metric.WithAttributes(sonnet))
	m.TokensTotal.Add(ctx, 868, metric.WithAttributes(sonnet, attribute.String("type", "cache_creation")))
	m.CostUSDTotal.Add(ctx, 0.0036, metric.WithAttributes(sonnet))
	m.SkillInvocationsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("skill", "summarize")))

	got := collectedMetrics(t, reader)[MeterClaude]
	if len(got) != 8 {
		names := make([]string, 0, len(got))
		for n := range got {
			names = append(names, n)
		}
		t.Fatalf("meter %q has %d instruments %v, want 8", MeterClaude, len(got), names)
	}
	for name, m := range got {
		if m.Description == "" {
			t.Errorf("%s has no description", name)
		}
	}

	runs := sumPoints[int64](t, got["claude_runs_total"])
	if runs[attrSet(sonnet, attribute.String("outcome", "success"))] != 2 || runs[attrSet(sonnet, attribute.String("outcome", "timeout"))] != 1 || len(runs) != 2 {
		t.Errorf("claude_runs_total = %v", runs)
	}
	for name, unit := range map[string]string{"claude_run_duration_seconds": "s", "claude_time_to_first_token_seconds": "s", "claude_queue_wait_seconds": "s"} {
		m := got[name]
		if m.Unit != unit {
			t.Errorf("%s unit = %q, want %q", name, m.Unit, unit)
		}
		pts := histPoints(t, m)
		dp, ok := pts[attrSet(sonnet)]
		if !ok || dp.Count != 1 {
			t.Errorf("%s points = %v", name, pts)
		}
	}
	// Bucket layout is part of the dashboard contract.
	if b := histPoints(t, got["claude_run_duration_seconds"])[attrSet(sonnet)].Bounds; !slices.Equal(b, []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300}) {
		t.Errorf("run duration buckets = %v", b)
	}
	if b := histPoints(t, got["claude_time_to_first_token_seconds"])[attrSet(sonnet)].Bounds; !slices.Equal(b, []float64{0.25, 0.5, 1, 2, 5, 10, 20, 30, 60}) {
		t.Errorf("ttft buckets = %v", b)
	}
	if b := histPoints(t, got["claude_queue_wait_seconds"])[attrSet(sonnet)].Bounds; !slices.Equal(b, []float64{0.01, 0.1, 0.5, 1, 2, 5, 10, 30, 60}) {
		t.Errorf("queue wait buckets = %v", b)
	}
	if v := sumPoints[int64](t, got["claude_tokens_total"])[attrSet(sonnet, attribute.String("type", "cache_creation"))]; v != 868 {
		t.Errorf("claude_tokens_total = %d", v)
	}
	if v := sumPoints[float64](t, got["claude_cost_usd_total"])[attrSet(sonnet)]; v != 0.0036 {
		t.Errorf("claude_cost_usd_total = %v", v)
	}
	if v := sumPoints[int64](t, got["claude_skill_invocations_total"])[attrSet(attribute.String("skill", "summarize"))]; v != 1 {
		t.Errorf("claude_skill_invocations_total = %d", v)
	}
	if v := gaugePoints(t, got["claude_runs_in_flight"])[attrSet()]; v != 3 {
		t.Errorf("claude_runs_in_flight = %d, want 3", v)
	}
}

func TestClaudeMetricsInFlightIsReadAtCollectionTime(t *testing.T) {
	mp, reader := newManualMeterProvider(t)
	m, _ := NewClaudeMetrics(mp)
	var cur int64
	if err := m.RegisterInFlight(func() int64 { return cur }); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int64{0, 4, 1} {
		cur = want
		if got := gaugePoints(t, collectedMetrics(t, reader)[MeterClaude]["claude_runs_in_flight"])[attrSet()]; got != want {
			t.Errorf("in flight = %d, want %d", got, want)
		}
	}
}

func TestSkillsMetricsInstruments(t *testing.T) {
	ctx := context.Background()
	mp, reader := newManualMeterProvider(t)
	m, err := NewSkillsMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	state := SkillsState{HaveSnapshot: true, Commit: "aaaaaaaaaaaa", Ref: "main", Loaded: 2, Skipped: 1, ConsecutiveFailures: 3, LastSuccess: time.Unix(1700000000, 0)}
	if err := m.RegisterState(func() SkillsState { return state }); err != nil {
		t.Fatal(err)
	}
	m.SyncTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "success")))
	m.SyncTotal.Add(ctx, 2, metric.WithAttributes(attribute.String("outcome", "noop")))
	m.SyncDuration.Record(ctx, 1.5)

	got := collectedMetrics(t, reader)[MeterSkills]
	if len(got) != 7 {
		t.Fatalf("meter %q has %d instruments, want 7", MeterSkills, len(got))
	}
	sync := sumPoints[int64](t, got["skills_sync_total"])
	if sync[attrSet(attribute.String("outcome", "success"))] != 1 || sync[attrSet(attribute.String("outcome", "noop"))] != 2 {
		t.Errorf("skills_sync_total = %v", sync)
	}
	if dp := histPoints(t, got["skills_sync_duration_seconds"])[attrSet()]; dp.Count != 1 || !slices.Equal(dp.Bounds, []float64{0.1, 0.5, 1, 2, 5, 10, 20, 60}) {
		t.Errorf("skills_sync_duration_seconds = %+v", dp)
	}
	if got["skills_sync_duration_seconds"].Unit != "s" {
		t.Errorf("sync duration unit = %q", got["skills_sync_duration_seconds"].Unit)
	}
	for name, want := range map[string]int64{"skills_loaded": 2, "skills_skipped": 1, "skills_sync_consecutive_failures": 3} {
		if v := gaugePoints(t, got[name])[attrSet()]; v != want {
			t.Errorf("%s = %d, want %d", name, v, want)
		}
	}
	if v := floatGaugePoints(t, got["skills_last_success_timestamp_seconds"])[attrSet()]; v != 1700000000 {
		t.Errorf("skills_last_success_timestamp_seconds = %v", v)
	}
	info := gaugePoints(t, got["skills_info"])
	if len(info) != 1 || info[attrSet(attribute.String("commit", "aaaaaaaaaaaa"), attribute.String("ref", "main"))] != 1 {
		t.Errorf("skills_info = %v", info)
	}

	// A new snapshot replaces the info series instead of accumulating a second one.
	state.Commit = "bbbbbbbbbbbb"
	info = gaugePoints(t, collectedMetrics(t, reader)[MeterSkills]["skills_info"])
	if len(info) != 1 || info[attrSet(attribute.String("commit", "bbbbbbbbbbbb"), attribute.String("ref", "main"))] != 1 {
		t.Errorf("skills_info after a new commit = %v, want only the current commit", info)
	}

	// Without a snapshot there is no info series and no last-success sample.
	state = SkillsState{ConsecutiveFailures: 1}
	got = collectedMetrics(t, reader)[MeterSkills]
	if m, ok := got["skills_info"]; ok && len(gaugePoints(t, m)) != 0 {
		t.Errorf("skills_info without a snapshot = %v", gaugePoints(t, m))
	}
	if m, ok := got["skills_last_success_timestamp_seconds"]; ok && len(floatGaugePoints(t, m)) != 0 {
		t.Errorf("last success timestamp without a success = %v", floatGaugePoints(t, m))
	}
	if v := gaugePoints(t, got["skills_sync_consecutive_failures"])[attrSet()]; v != 1 {
		t.Errorf("consecutive failures = %d", v)
	}
}

// ---------------------------------------------------------------------------
// Prometheus exposition: the exact names Grafana queries.
// ---------------------------------------------------------------------------

// scrape renders the registry the way promhttp serves the internal /metrics.
func scrape(t *testing.T, reg *prom.Registry) string {
	t.Helper()
	srv := httptest.NewServer(promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// exposedFamilies returns "name type" pairs from the # TYPE lines.
func exposedFamilies(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(l, "# TYPE "); ok {
			out = append(out, rest)
		}
	}
	sort.Strings(out)
	return out
}

func TestPrometheusExpositionNames(t *testing.T) {
	ctx := context.Background()
	reg := prom.NewRegistry()
	exp, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		t.Fatal(err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp))
	t.Cleanup(func() { _ = mp.Shutdown(ctx) })

	cm, err := NewClaudeMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	if err := cm.RegisterInFlight(func() int64 { return 1 }); err != nil {
		t.Fatal(err)
	}
	sm, err := NewSkillsMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	if err := sm.RegisterState(func() SkillsState {
		return SkillsState{HaveSnapshot: true, Commit: "0123456789ab", Ref: "main", Loaded: 1, LastSuccess: time.Unix(1700000000, 0)}
	}); err != nil {
		t.Fatal(err)
	}

	model := attribute.String("model", "claude-haiku-5-5")
	cm.RunsTotal.Add(ctx, 1, metric.WithAttributes(model, attribute.String("outcome", "success")))
	cm.RunDuration.Record(ctx, 3, metric.WithAttributes(model))
	cm.TimeToFirstToken.Record(ctx, 1, metric.WithAttributes(model))
	cm.QueueWait.Record(ctx, 0, metric.WithAttributes(model))
	cm.TokensTotal.Add(ctx, 10, metric.WithAttributes(model, attribute.String("type", "output")))
	cm.CostUSDTotal.Add(ctx, 0.001, metric.WithAttributes(model))
	cm.SkillInvocationsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("skill", "summarize")))
	sm.SyncTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "success")))
	sm.SyncDuration.Record(ctx, 0.4)

	text := scrape(t, reg)

	// These are the names T7.2 (Grafana) builds PromQL on.
	want := []string{
		"claude_cost_usd_total counter",
		"claude_queue_wait_seconds histogram",
		"claude_run_duration_seconds histogram",
		"claude_runs_in_flight gauge",
		"claude_runs_total counter",
		"claude_skill_invocations_total counter",
		"claude_time_to_first_token_seconds histogram",
		"claude_tokens_total counter",
		"skills_info gauge",
		"skills_last_success_timestamp_seconds gauge",
		"skills_loaded gauge",
		"skills_skipped gauge",
		"skills_sync_consecutive_failures gauge",
		"skills_sync_duration_seconds histogram",
		"skills_sync_total counter",
	}
	var got []string
	for _, f := range exposedFamilies(text) {
		if strings.HasPrefix(f, "claude_") || strings.HasPrefix(f, "skills_") {
			got = append(got, f)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("exposed claude_/skills_ families:\n got %v\nwant %v", got, want)
	}
	for _, bad := range []string{"_total_total", "_seconds_seconds", "_usd_usd", "_ratio"} {
		if strings.Contains(text, bad) {
			t.Errorf("exposition contains %q:\n%s", bad, text)
		}
	}

	// Sample lines (labels sorted by the exporter; the otel_scope_* labels are
	// added by the exporter and ignored by the queries).
	for _, line := range []string{
		`claude_runs_total{model="claude-haiku-5-5",otel_scope_name="claudeai-api/claude",otel_scope_schema_url="",otel_scope_version="",outcome="success"} 1`,
		`claude_runs_in_flight{otel_scope_name="claudeai-api/claude",otel_scope_schema_url="",otel_scope_version=""} 1`,
		`claude_run_duration_seconds_bucket{model="claude-haiku-5-5",otel_scope_name="claudeai-api/claude",otel_scope_schema_url="",otel_scope_version="",le="5"} 1`,
		`claude_run_duration_seconds_count{model="claude-haiku-5-5",otel_scope_name="claudeai-api/claude",otel_scope_schema_url="",otel_scope_version=""} 1`,
		`claude_tokens_total{model="claude-haiku-5-5",otel_scope_name="claudeai-api/claude",otel_scope_schema_url="",otel_scope_version="",type="output"} 10`,
		`claude_cost_usd_total{model="claude-haiku-5-5",otel_scope_name="claudeai-api/claude",otel_scope_schema_url="",otel_scope_version=""} 0.001`,
		`claude_skill_invocations_total{otel_scope_name="claudeai-api/claude",otel_scope_schema_url="",otel_scope_version="",skill="summarize"} 1`,
		`skills_sync_total{otel_scope_name="claudeai-api/skills",otel_scope_schema_url="",otel_scope_version="",outcome="success"} 1`,
		`skills_info{commit="0123456789ab",otel_scope_name="claudeai-api/skills",otel_scope_schema_url="",otel_scope_version="",ref="main"} 1`,
		`skills_loaded{otel_scope_name="claudeai-api/skills",otel_scope_schema_url="",otel_scope_version=""} 1`,
	} {
		if !strings.Contains(text, line+"\n") {
			t.Errorf("exposition lacks sample line:\n  %s\nfull claude/skills output:\n%s", line, filterLines(text, "claude_", "skills_"))
		}
	}
}

func filterLines(text string, prefixes ...string) string {
	var b strings.Builder
	for _, l := range strings.Split(text, "\n") {
		for _, p := range prefixes {
			if strings.HasPrefix(l, p) || strings.HasPrefix(l, "# TYPE "+p) {
				b.WriteString(l + "\n")
				break
			}
		}
	}
	return b.String()
}
