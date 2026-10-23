package app

import (
	"context"
	"testing"
	"time"

	"github.com/martinezsaweczko/claudeai-api/config"
	"github.com/martinezsaweczko/claudeai-api/o11"
	"github.com/martinezsaweczko/claudeai-api/services/claude"
	"github.com/martinezsaweczko/claudeai-api/services/skills"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestModelNormalizer(t *testing.T) {
	n := newModelNormalizer(config.ClaudeConfig{Model: "sonnet", Models: []string{"opus", "haiku"}})
	tests := []struct {
		name, resolved, requested, want string
	}{
		{"resolved id wins", "claude-sonnet-5-5", "sonnet", "claude-sonnet-5-5"},
		{"resolved id wins over an unknown request", "claude-haiku-5-5", "gpt-4o", "claude-haiku-5-5"},
		{"advertised alias without a result", "", "opus", "opus"},
		{"default model is allowed even if not advertised", "", "sonnet", "sonnet"},
		{"unknown request", "", "gpt-4o", "other"},
		{"empty request and no result", "", "", "other"},
		{"client string with label-breaking characters", "", "x\"} injected{a=\"b", "other"},
		{"case matters", "", "Opus", "other"},
		{"malformed resolved id falls back to the request", "bad id with spaces", "haiku", "haiku"},
		{"malformed resolved id and unknown request", "x\ny", "nope", "other"},
		{"overlong resolved id", string(make([]byte, 200)), "", "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := n.label(tt.resolved, tt.requested); got != tt.want {
				t.Errorf("label(%q, %q) = %q, want %q", tt.resolved, tt.requested, got, tt.want)
			}
		})
	}

	// Without a configured list the default model is the only advertised one.
	n2 := newModelNormalizer(config.ClaudeConfig{Model: "sonnet"})
	if n2.label("", "sonnet") != "sonnet" || n2.label("", "opus") != "other" {
		t.Error("default-only normalizer wrong")
	}
}

func collectAll(t *testing.T, r *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func TestClaudeRunObserverRecordsBoundedLabels(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	cm, err := o11.NewClaudeMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	obs := claudeRunObserver(cm, newModelNormalizer(config.ClaudeConfig{Model: "sonnet"}), func() []string { return []string{"summarize"} })

	obs(claude.RunInfo{
		Model: "gpt-9", ResolvedModel: "", Outcome: "brand-new-outcome", Duration: 3 * time.Second,
		SkillsInvoked: []string{"summarize", "summarize", "<script>"},
		WebSearches:   3, WebFetches: 1,
	})
	obs(claude.RunInfo{
		Model: "sonnet", ResolvedModel: "claude-sonnet-5-5", Outcome: claude.OutcomeSuccess, Duration: 2 * time.Second,
		QueueWait: 0, FirstToken: true, TTFT: time.Second, CostUSD: 0.5, WebSearches: 2,
		Usage: claude.Usage{InputTokens: 1, OutputTokens: 2, CacheCreationTokens: 3, CacheReadTokens: 4},
	})

	got := collectAll(t, reader)
	runs := got["claude_runs_total"].Data.(metricdata.Sum[int64])
	seen := map[[2]string]int64{}
	for _, dp := range runs.DataPoints {
		model, _ := dp.Attributes.Value("model")
		outcome, _ := dp.Attributes.Value("outcome")
		seen[[2]string{model.AsString(), outcome.AsString()}] = dp.Value
	}
	if len(seen) != 2 || seen[[2]string{"other", "other"}] != 1 || seen[[2]string{"claude-sonnet-5-5", "success"}] != 1 {
		t.Errorf("claude_runs_total = %v", seen)
	}

	skillsSum := got["claude_skill_invocations_total"].Data.(metricdata.Sum[int64])
	bySkill := map[string]int64{}
	for _, dp := range skillsSum.DataPoints {
		v, _ := dp.Attributes.Value(attribute.Key("skill"))
		bySkill[v.AsString()] = dp.Value
	}
	if len(bySkill) != 2 || bySkill["summarize"] != 2 || bySkill["other"] != 1 {
		t.Errorf("skill invocations = %v", bySkill)
	}

	web := got["claude_web_requests_total"].Data.(metricdata.Sum[int64])
	byWeb := map[string]int64{}
	for _, dp := range web.DataPoints {
		v, _ := dp.Attributes.Value("type")
		byWeb[v.AsString()] = dp.Value
	}
	if len(byWeb) != 2 || byWeb["search"] != 5 || byWeb["fetch"] != 1 {
		t.Errorf("web requests = %v", byWeb)
	}

	// TTFT only for the run that produced text; queue wait and duration for both.
	count := func(name string) uint64 {
		var c uint64
		for _, dp := range got[name].Data.(metricdata.Histogram[float64]).DataPoints {
			c += dp.Count
		}
		return c
	}
	if count("claude_time_to_first_token_seconds") != 1 || count("claude_queue_wait_seconds") != 2 || count("claude_run_duration_seconds") != 2 {
		t.Errorf("histogram counts ttft=%d queue=%d duration=%d", count("claude_time_to_first_token_seconds"), count("claude_queue_wait_seconds"), count("claude_run_duration_seconds"))
	}
	tokens := got["claude_tokens_total"].Data.(metricdata.Sum[int64])
	byType := map[string]int64{}
	for _, dp := range tokens.DataPoints {
		v, _ := dp.Attributes.Value("type")
		byType[v.AsString()] = dp.Value
	}
	if len(byType) != 4 || byType["input"] != 1 || byType["output"] != 2 || byType["cache_creation"] != 3 || byType["cache_read"] != 4 {
		t.Errorf("tokens = %v", byType)
	}
	if c := got["claude_cost_usd_total"].Data.(metricdata.Sum[float64]); len(c.DataPoints) != 1 || c.DataPoints[0].Value != 0.5 {
		t.Errorf("cost = %+v", c)
	}
}

func TestSkillsStateFuncReflectsSyncer(t *testing.T) {
	synced := time.Unix(1700000000, 0)
	f := &statusSyncer{
		snap:   skills.Snapshot{Commit: "0123456789abcdef0123", Ref: "v1", Skills: make([]skills.SkillInfo, 3), Skipped: make([]skills.Skipped, 2)},
		have:   true,
		status: skills.Status{ConsecutiveFailures: 2, LastSuccess: synced},
	}
	got := skillsStateFunc(f)()
	want := o11.SkillsState{HaveSnapshot: true, Commit: "0123456789ab", Ref: "v1", Loaded: 3, Skipped: 2, ConsecutiveFailures: 2, LastSuccess: synced}
	if got != want {
		t.Errorf("state = %+v, want %+v", got, want)
	}
	if got := skillsStateFunc(&statusSyncer{status: skills.Status{ConsecutiveFailures: 1}})(); got.HaveSnapshot || got.ConsecutiveFailures != 1 {
		t.Errorf("state without a snapshot = %+v", got)
	}
	if names := currentSkillNames(&statusSyncer{})(); names != nil {
		t.Errorf("names without a snapshot = %v", names)
	}
}

type statusSyncer struct {
	snap   skills.Snapshot
	have   bool
	status skills.Status
}

func (s *statusSyncer) Current() (skills.Snapshot, bool) { return s.snap, s.have }
func (s *statusSyncer) Status() skills.Status            { return s.status }

func TestSkillsObserverSurvivesNilishInput(t *testing.T) {
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))
	sm, err := o11.NewSkillsMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	obs := newSkillsObserver(sm)
	obs(skills.SyncInfo{}) // zero value: counted as an error, never panics
}
