package o11

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Meter names for the Claude CLI backend and the skills syncer.
const (
	MeterClaude = "claudeai-api/claude"
	MeterSkills = "claudeai-api/skills"
)

// Histogram buckets (seconds), sized for the ranges each signal really has.
var (
	// claude -p calls take from about a second to the configured timeout.
	runDurationBuckets = []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300}
	// First text delta; a skill tool call adds a whole extra model turn.
	ttftBuckets = []float64{0.25, 0.5, 1, 2, 5, 10, 20, 30, 60}
	// Waiting for a free concurrency slot; most runs wait 0.
	queueWaitBuckets = []float64{0.01, 0.1, 0.5, 1, 2, 5, 10, 30, 60}
	// A git fetch/clone plus materialization.
	syncDurationBuckets = []float64{0.1, 0.5, 1, 2, 5, 10, 20, 60}
)

// ClaudeMetrics holds the instruments for Claude CLI runs.
//
// Label rules (see the Metrics section of the README): `model` must be a
// bounded value (the CLI-resolved model id, or the requested model only when it
// is in the advertised list, else "other"), `outcome` one of the claude.Outcome*
// values plus "other", `type` one of input|output|cache_creation|cache_read and
// `skill` a loaded skill name or "other". Callers own that normalization:
// client-controlled strings must never reach a label.
type ClaudeMetrics struct {
	RunsTotal             metric.Int64Counter         // claude_runs_total{model,outcome}
	RunDuration           metric.Float64Histogram     // claude_run_duration_seconds{model}
	TimeToFirstToken      metric.Float64Histogram     // claude_time_to_first_token_seconds{model}
	QueueWait             metric.Float64Histogram     // claude_queue_wait_seconds{model}
	TokensTotal           metric.Int64Counter         // claude_tokens_total{model,type}
	CostUSDTotal          metric.Float64Counter       // claude_cost_usd_total{model}
	SkillInvocationsTotal metric.Int64Counter         // claude_skill_invocations_total{skill}
	inFlight              metric.Int64ObservableGauge // claude_runs_in_flight

	meter metric.Meter
}

func NewClaudeMetrics(mp metric.MeterProvider) (*ClaudeMetrics, error) {
	m := mp.Meter(MeterClaude)
	c := &ClaudeMetrics{meter: m}
	var err error

	if c.RunsTotal, err = m.Int64Counter("claude_runs_total",
		metric.WithDescription("Claude CLI runs by normalized model and outcome")); err != nil {
		return nil, err
	}
	if c.RunDuration, err = m.Float64Histogram("claude_run_duration_seconds",
		metric.WithDescription("Total duration of a Claude run, including the wait for a concurrency slot"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(runDurationBuckets...)); err != nil {
		return nil, err
	}
	if c.TimeToFirstToken, err = m.Float64Histogram("claude_time_to_first_token_seconds",
		metric.WithDescription("Time from the start of the call to the first text delta (runs that produced text only)"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(ttftBuckets...)); err != nil {
		return nil, err
	}
	if c.QueueWait, err = m.Float64Histogram("claude_queue_wait_seconds",
		metric.WithDescription("Time a run waited for a concurrency slot (0 when one was free)"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(queueWaitBuckets...)); err != nil {
		return nil, err
	}
	if c.TokensTotal, err = m.Int64Counter("claude_tokens_total",
		metric.WithDescription("Tokens reported by the CLI result, summed over all turns of a run, by type (input|output|cache_creation|cache_read)")); err != nil {
		return nil, err
	}
	if c.CostUSDTotal, err = m.Float64Counter("claude_cost_usd_total",
		metric.WithDescription("Estimated cost in USD reported by the CLI (total_cost_usd); an estimate, not billing")); err != nil {
		return nil, err
	}
	if c.SkillInvocationsTotal, err = m.Int64Counter("claude_skill_invocations_total",
		metric.WithDescription("Skill tool uses by skill name (names outside the loaded snapshot are reported as other)")); err != nil {
		return nil, err
	}
	if c.inFlight, err = m.Int64ObservableGauge("claude_runs_in_flight",
		metric.WithDescription("Claude CLI processes currently running (queued callers are not counted)")); err != nil {
		return nil, err
	}
	return c, nil
}

// RegisterInFlight makes claude_runs_in_flight report fn at every collection.
func (c *ClaudeMetrics) RegisterInFlight(fn func() int64) error {
	_, err := c.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(c.inFlight, fn())
		return nil
	}, c.inFlight)
	return err
}

// SkillsMetrics holds the instruments for the skills syncer.
type SkillsMetrics struct {
	SyncTotal    metric.Int64Counter     // skills_sync_total{outcome=success|noop|error}
	SyncDuration metric.Float64Histogram // skills_sync_duration_seconds

	loaded      metric.Int64ObservableGauge   // skills_loaded
	skipped     metric.Int64ObservableGauge   // skills_skipped
	failures    metric.Int64ObservableGauge   // skills_sync_consecutive_failures
	lastSuccess metric.Float64ObservableGauge // skills_last_success_timestamp_seconds
	info        metric.Int64ObservableGauge   // skills_info{commit,ref}

	meter metric.Meter
}

// SkillsState is the point-in-time view read at every collection.
type SkillsState struct {
	HaveSnapshot        bool // a snapshot is loaded; Commit and Ref are meaningful (Loaded/Skipped are 0 otherwise)
	Commit              string
	Ref                 string
	Loaded              int
	Skipped             int
	ConsecutiveFailures int
	LastSuccess         time.Time // zero when no sync has succeeded
}

func NewSkillsMetrics(mp metric.MeterProvider) (*SkillsMetrics, error) {
	m := mp.Meter(MeterSkills)
	s := &SkillsMetrics{meter: m}
	var err error

	if s.SyncTotal, err = m.Int64Counter("skills_sync_total",
		metric.WithDescription("Skills repository syncs by outcome (success: new version, noop: already current, error)")); err != nil {
		return nil, err
	}
	if s.SyncDuration, err = m.Float64Histogram("skills_sync_duration_seconds",
		metric.WithDescription("Duration of a skills repository sync"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(syncDurationBuckets...)); err != nil {
		return nil, err
	}
	if s.loaded, err = m.Int64ObservableGauge("skills_loaded",
		metric.WithDescription("Skills in the current snapshot")); err != nil {
		return nil, err
	}
	if s.skipped, err = m.Int64ObservableGauge("skills_skipped",
		metric.WithDescription("Skills in the repository ignored by validation in the current snapshot")); err != nil {
		return nil, err
	}
	if s.failures, err = m.Int64ObservableGauge("skills_sync_consecutive_failures",
		metric.WithDescription("Consecutive failed skills syncs (0 after a success)")); err != nil {
		return nil, err
	}
	if s.lastSuccess, err = m.Float64ObservableGauge("skills_last_success_timestamp_seconds",
		metric.WithDescription("Unix time of the last successful skills sync")); err != nil {
		return nil, err
	}
	if s.info, err = m.Int64ObservableGauge("skills_info",
		metric.WithDescription("Constant 1 labelled with the commit and ref of the CURRENT snapshot only")); err != nil {
		return nil, err
	}
	return s, nil
}

// RegisterState makes the skills gauges report fn at every collection. Only the
// current snapshot is ever reported in skills_info, so replaced commits do not
// accumulate series.
func (s *SkillsMetrics) RegisterState(fn func() SkillsState) error {
	_, err := s.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		st := fn()
		o.ObserveInt64(s.failures, int64(st.ConsecutiveFailures))
		if !st.LastSuccess.IsZero() {
			o.ObserveFloat64(s.lastSuccess, float64(st.LastSuccess.UnixNano())/1e9)
		}
		o.ObserveInt64(s.loaded, int64(st.Loaded))
		o.ObserveInt64(s.skipped, int64(st.Skipped))
		if st.HaveSnapshot {
			o.ObserveInt64(s.info, 1, metric.WithAttributes(
				attribute.String("commit", st.Commit),
				attribute.String("ref", st.Ref),
			))
		}
		return nil
	}, s.failures, s.lastSuccess, s.loaded, s.skipped, s.info)
	return err
}
