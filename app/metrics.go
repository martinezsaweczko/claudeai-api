package app

import (
	"context"
	"regexp"
	"slices"

	"github.com/martinezsaweczko/claudeai-api/config"
	"github.com/martinezsaweczko/claudeai-api/o11"
	"github.com/martinezsaweczko/claudeai-api/services/claude"
	"github.com/martinezsaweczko/claudeai-api/services/skills"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// otherLabel is the single value every unbounded or unknown label collapses to.
const otherLabel = "other"

// runOutcomes is the bounded set of claude_runs_total outcome values.
var runOutcomes = map[string]bool{
	claude.OutcomeSuccess: true, claude.OutcomeAuth: true, claude.OutcomeRateLimited: true,
	claude.OutcomeTimeout: true, claude.OutcomeBusy: true, claude.OutcomeCLIError: true,
	claude.OutcomeNotFound: true, claude.OutcomeInvalidModel: true, claude.OutcomeInvalidRequest: true,
	claude.OutcomeCanceled: true,
}

// resolvedModelRe bounds what is accepted as a CLI-reported model id.
var resolvedModelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\-\[\]]{0,63}$`)

// modelNormalizer maps models to metric label values without ever letting a
// client-controlled string create a new series.
type modelNormalizer struct{ allowed map[string]bool }

// newModelNormalizer allows the advertised models plus the default one.
func newModelNormalizer(c config.ClaudeConfig) modelNormalizer {
	n := modelNormalizer{allowed: map[string]bool{}}
	for _, m := range advertisedModels(c) {
		n.allowed[m] = true
	}
	if c.Model != "" {
		n.allowed[c.Model] = true
	}
	return n
}

// label returns the CLI-resolved model id when there is one (a bounded set of
// real ids), otherwise the requested model if it is advertised, else "other".
func (n modelNormalizer) label(resolved, requested string) string {
	if resolved != "" && resolvedModelRe.MatchString(resolved) {
		return resolved
	}
	if n.allowed[requested] {
		return requested
	}
	return otherLabel
}

// claudeRunObserver turns finished runs into metrics. skillNames returns the
// names in the current skills snapshot (nil when skills are off).
func claudeRunObserver(m *o11.ClaudeMetrics, models modelNormalizer, skillNames func() []string) func(claude.RunInfo) {
	return func(i claude.RunInfo) {
		ctx := context.Background()
		model := attribute.String("model", models.label(i.ResolvedModel, i.Model))
		outcome := i.Outcome
		if !runOutcomes[outcome] {
			outcome = otherLabel
		}
		withModel := metric.WithAttributes(model)

		m.RunsTotal.Add(ctx, 1, metric.WithAttributes(model, attribute.String("outcome", outcome)))
		m.RunDuration.Record(ctx, i.Duration.Seconds(), withModel)
		m.QueueWait.Record(ctx, i.QueueWait.Seconds(), withModel)
		if i.FirstToken {
			m.TimeToFirstToken.Record(ctx, i.TTFT.Seconds(), withModel)
		}
		for typ, n := range map[string]int{
			"input":          i.Usage.InputTokens,
			"output":         i.Usage.OutputTokens,
			"cache_creation": i.Usage.CacheCreationTokens,
			"cache_read":     i.Usage.CacheReadTokens,
		} {
			if n > 0 {
				m.TokensTotal.Add(ctx, int64(n), metric.WithAttributes(model, attribute.String("type", typ)))
			}
		}
		if i.CostUSD > 0 {
			m.CostUSDTotal.Add(ctx, i.CostUSD, withModel)
		}
		if len(i.SkillsInvoked) > 0 {
			known := skillNames()
			for _, name := range i.SkillsInvoked {
				label := otherLabel
				if slices.Contains(known, name) {
					label = name
				}
				m.SkillInvocationsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("skill", label)))
			}
		}
	}
}

// newSkillsObserver turns finished syncs into metrics. It recovers from panics:
// the syncer calls it inline and a metrics bug must not break a sync.
func newSkillsObserver(m *o11.SkillsMetrics) func(skills.SyncInfo) {
	return func(i skills.SyncInfo) {
		defer func() { _ = recover() }()
		outcome := "error"
		switch i.Outcome {
		case skills.OutcomeUpdated:
			outcome = "success"
		case skills.OutcomeUnchanged:
			outcome = "noop"
		}
		ctx := context.Background()
		m.SyncTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
		m.SyncDuration.Record(ctx, i.Duration.Seconds())
	}
}

// skillsStateFunc reads the syncer at collection time.
func skillsStateFunc(src interface {
	Current() (skills.Snapshot, bool)
	Status() skills.Status
}) func() o11.SkillsState {
	return func() o11.SkillsState {
		st := src.Status()
		out := o11.SkillsState{ConsecutiveFailures: st.ConsecutiveFailures, LastSuccess: st.LastSuccess}
		if snap, ok := src.Current(); ok {
			out.HaveSnapshot, out.Commit, out.Ref = true, snap.Commit[:min(12, len(snap.Commit))], snap.Ref
			out.Loaded, out.Skipped = len(snap.Skills), len(snap.Skipped)
		}
		return out
	}
}

// currentSkillNames returns the names in the current snapshot.
func currentSkillNames(src interface {
	Current() (skills.Snapshot, bool)
}) func() []string {
	return func() []string {
		snap, ok := src.Current()
		if !ok {
			return nil
		}
		names := make([]string, len(snap.Skills))
		for i, s := range snap.Skills {
			names[i] = s.Name
		}
		return names
	}
}
