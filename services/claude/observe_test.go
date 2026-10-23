package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/martinezsaweczko/claudeai-api/config"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// infoSink collects RunInfo values delivered to the observer.
type infoSink struct {
	mu    sync.Mutex
	infos []RunInfo
}

func (s *infoSink) observe(i RunInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.infos = append(s.infos, i)
}

func (s *infoSink) all() []RunInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.infos)
}

func (s *infoSink) only(t *testing.T) RunInfo {
	t.Helper()
	got := s.all()
	if len(got) != 1 {
		t.Fatalf("observer got %d RunInfo values, want 1: %+v", len(got), got)
	}
	return got[0]
}

func TestRunInfoSuccess(t *testing.T) {
	sink := &infoSink{}
	b := newFakeBackend(t, config.ClaudeConfig{}, map[string]string{envScenario: "fixture", envFixture: fixturePath(t, "success.stream.jsonl")}, WithObserver(sink.observe))
	if _, err := b.Complete(context.Background(), simpleReq); err != nil {
		t.Fatal(err)
	}
	i := sink.only(t)
	if i.Outcome != OutcomeSuccess || i.Err != nil || i.ExitCode != 0 {
		t.Errorf("outcome/err/exit = %q %v %d", i.Outcome, i.Err, i.ExitCode)
	}
	if i.Model != "sonnet" || i.ResolvedModel != "claude-sonnet-5-5" {
		t.Errorf("model = %q resolved = %q", i.Model, i.ResolvedModel)
	}
	if want := (Usage{InputTokens: 2, OutputTokens: 4, CacheCreationTokens: 868}); i.Usage != want {
		t.Errorf("usage = %+v, want %+v", i.Usage, want)
	}
	if i.CostUSD <= 0 || i.NumTurns != 1 {
		t.Errorf("cost = %v turns = %d", i.CostUSD, i.NumTurns)
	}
	if !i.FirstToken || i.TTFT <= 0 || i.TTFT > i.Duration {
		t.Errorf("ttft = %v (first=%v) duration = %v", i.TTFT, i.FirstToken, i.Duration)
	}
	if i.SkillsActive || len(i.SkillsInvoked) != 0 || i.SkillsCommit != "" {
		t.Errorf("skills fields set without skills: %+v", i)
	}
}

func TestRunInfoErrors(t *testing.T) {
	tests := []struct {
		name      string
		cfg       config.ClaudeConfig
		scenario  map[string]string
		wantOut   string
		wantUsage bool // usage from an error result is still reported
		wantFirst bool
	}{
		{"auth", config.ClaudeConfig{}, map[string]string{envScenario: "fixture", envFixture: fixturePath(t, "auth_error.stream.jsonl"), envExit: "1"}, OutcomeAuth, false, false},
		{"rate limited keeps usage", config.ClaudeConfig{}, map[string]string{envScenario: "fixture", envFixture: fixturePath(t, "rate_limited.stream.jsonl"), envExit: "1"}, OutcomeRateLimited, true, false},
		{"invalid model", config.ClaudeConfig{}, map[string]string{envScenario: "fixture", envFixture: fixturePath(t, "invalid_model.stream.jsonl"), envExit: "1"}, OutcomeInvalidModel, false, false},
		{"truncated stream", config.ClaudeConfig{}, map[string]string{envScenario: "fixture", envFixture: fixturePath(t, "truncated.stream.jsonl")}, OutcomeCLIError, false, true},
		{"timeout after a delta", config.ClaudeConfig{Timeout: 300 * time.Millisecond}, map[string]string{envScenario: "hang", envDir: t.TempDir()}, OutcomeTimeout, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &infoSink{}
			b := newFakeBackend(t, tt.cfg, tt.scenario, WithObserver(sink.observe))
			_, err := b.Complete(context.Background(), simpleReq)
			if err == nil {
				t.Fatal("want an error")
			}
			i := sink.only(t)
			if i.Outcome != tt.wantOut {
				t.Errorf("outcome = %q, want %q (err %v)", i.Outcome, tt.wantOut, err)
			}
			if !errors.Is(i.Err, err) && i.Err != err {
				t.Errorf("RunInfo.Err = %v, want %v", i.Err, err)
			}
			if got := i.Usage != (Usage{}); got != tt.wantUsage {
				t.Errorf("usage reported = %v (%+v), want %v", got, i.Usage, tt.wantUsage)
			}
			if i.FirstToken != tt.wantFirst {
				t.Errorf("FirstToken = %v, want %v", i.FirstToken, tt.wantFirst)
			}
			if i.Duration <= 0 {
				t.Errorf("duration = %v", i.Duration)
			}
		})
	}
}

func TestRunInfoNotFoundAndInvalidRequest(t *testing.T) {
	sink := &infoSink{}
	b := NewCLIBackend(config.ClaudeConfig{Bin: "/nonexistent/claude-bin", Model: "sonnet", Timeout: time.Second, MaxConcurrency: 1}, WithWorkDir(t.TempDir()), WithObserver(sink.observe))
	if _, err := b.Complete(context.Background(), simpleReq); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := b.Complete(context.Background(), Request{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatal(err)
	}
	got := sink.all()
	if len(got) != 2 || got[0].Outcome != OutcomeNotFound || got[1].Outcome != OutcomeInvalidRequest {
		t.Fatalf("infos = %+v", got)
	}
	if got[1].QueueWait != 0 {
		t.Errorf("a request rejected before queueing waited %v", got[1].QueueWait)
	}
}

func TestRunInfoBusyAndCanceled(t *testing.T) {
	dir := t.TempDir()
	sink := &infoSink{}
	b := newFakeBackend(t, config.ClaudeConfig{MaxConcurrency: 1}, map[string]string{
		envScenario: "block", envFixture: fixturePath(t, "success.stream.jsonl"), envDir: dir,
	}, WithObserver(sink.observe))

	first := make(chan error, 1)
	go func() { _, err := b.Complete(context.Background(), simpleReq); first <- err }()
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "started")); return err == nil })
	if got := b.InFlight(); got != 1 {
		t.Errorf("InFlight while running = %d, want 1", got)
	}

	// Busy: the queue wait ends by deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if _, err := b.Complete(ctx, simpleReq); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v", err)
	}
	// Canceled while queued: the CLI wraps context.Canceled in ErrBusy, so the
	// outcome is busy as well (the slot was never obtained).
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { time.Sleep(40 * time.Millisecond); cancel2() }()
	if _, err := b.Complete(ctx2, simpleReq); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if got := b.InFlight(); got != 1 {
		t.Errorf("queued callers must not count as in flight: %d", got)
	}

	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.InFlight() == 0 })

	got := sink.all()
	if len(got) != 3 {
		t.Fatalf("infos = %d, want 3", len(got))
	}
	busy, canceledQueued, ok := got[0], got[1], got[2]
	if busy.Outcome != OutcomeBusy || busy.QueueWait < 100*time.Millisecond || busy.ExitCode != -1 {
		t.Errorf("busy = %+v", busy)
	}
	if canceledQueued.Outcome != OutcomeBusy || canceledQueued.QueueWait < 30*time.Millisecond {
		t.Errorf("queued+canceled = %+v", canceledQueued)
	}
	if ok.Outcome != OutcomeSuccess {
		t.Errorf("first = %+v", ok)
	}
}

func TestRunInfoCanceledWhileRunning(t *testing.T) {
	dir := t.TempDir()
	sink := &infoSink{}
	b := newFakeBackend(t, config.ClaudeConfig{}, map[string]string{envScenario: "hang", envDir: dir}, WithObserver(sink.observe))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "pids")); return err == nil })
		cancel()
	}()
	if _, err := b.Complete(ctx, simpleReq); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if i := sink.only(t); i.Outcome != OutcomeCanceled {
		t.Errorf("outcome = %q, want canceled", i.Outcome)
	}
}

func TestQueueWaitMeasured(t *testing.T) {
	dir := t.TempDir()
	sink := &infoSink{}
	b := newFakeBackend(t, config.ClaudeConfig{MaxConcurrency: 1}, map[string]string{
		envScenario: "block", envFixture: fixturePath(t, "success.stream.jsonl"), envDir: dir,
	}, WithObserver(sink.observe))

	done := make(chan error, 2)
	go func() { _, err := b.Complete(context.Background(), simpleReq); done <- err }()
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "started")); return err == nil })
	go func() { _, err := b.Complete(context.Background(), simpleReq); done <- err }()
	time.Sleep(250 * time.Millisecond) // the second caller is queued meanwhile
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}

	got := sink.all()
	if len(got) != 2 {
		t.Fatalf("infos = %d", len(got))
	}
	// Completion order: the first run finishes (and reports) before the queued one starts.
	first, second := got[0], got[1]
	if first.QueueWait > 100*time.Millisecond {
		t.Errorf("uncontended run waited %v", first.QueueWait)
	}
	if second.QueueWait < 200*time.Millisecond {
		t.Errorf("queued run waited %v, want >= 200ms", second.QueueWait)
	}
	if second.Duration < second.QueueWait {
		t.Errorf("duration %v must include the queue wait %v", second.Duration, second.QueueWait)
	}
}

func TestTTFTMeasured(t *testing.T) {
	sink := &infoSink{}
	b := newFakeBackend(t, config.ClaudeConfig{}, map[string]string{
		envScenario: "delayed", envDelayMS: "250", envFixture: fixturePath(t, "success.stream.jsonl"),
	}, WithObserver(sink.observe))
	if _, err := b.Complete(context.Background(), simpleReq); err != nil {
		t.Fatal(err)
	}
	i := sink.only(t)
	if !i.FirstToken || i.TTFT < 250*time.Millisecond || i.TTFT > i.Duration {
		t.Errorf("ttft = %v first = %v duration = %v", i.TTFT, i.FirstToken, i.Duration)
	}
}

func TestObserverPanicDoesNotBreakRun(t *testing.T) {
	b := newFakeBackend(t, config.ClaudeConfig{}, map[string]string{envScenario: "fixture", envFixture: fixturePath(t, "success.stream.jsonl")},
		WithObserver(func(RunInfo) { panic("metrics bug") }))
	res, err := b.Complete(context.Background(), simpleReq)
	if err != nil || res.Text != "OK" {
		t.Fatalf("run broken by a panicking observer: %+v %v", res, err)
	}
	// And the slot was released: another run still works.
	if _, err := b.Complete(context.Background(), simpleReq); err != nil {
		t.Fatalf("second run: %v", err)
	}
}

func TestNoObserverStillWorks(t *testing.T) {
	b := newFakeBackend(t, config.ClaudeConfig{}, map[string]string{envScenario: "fixture", envFixture: fixturePath(t, "success.stream.jsonl")})
	if _, err := b.Complete(context.Background(), simpleReq); err != nil {
		t.Fatal(err)
	}
}

func TestSkillInvocationReportedFromRealFixture(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	snap := filepath.Join(t.TempDir(), "versions", sha)
	if err := os.MkdirAll(snap, 0o700); err != nil {
		t.Fatal(err)
	}
	sink := &infoSink{}
	b := newFakeBackend(t, config.ClaudeConfig{}, map[string]string{envScenario: "fixture", envFixture: fixturePath(t, "skill_invocation.stream.jsonl")},
		WithSkills(func() (string, bool) { return snap, true }), WithObserver(sink.observe))
	res, err := b.Complete(context.Background(), simpleReq)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.SkillsInvoked, []string{"summarize"}) || res.NumTurns != 3 {
		t.Errorf("result skills = %v turns = %d", res.SkillsInvoked, res.NumTurns)
	}
	i := sink.only(t)
	if !i.SkillsActive || i.SkillsCommit != sha[:12] || !reflect.DeepEqual(i.SkillsInvoked, []string{"summarize"}) || i.NumTurns != 3 {
		t.Errorf("RunInfo skills = %+v", i)
	}
	if i.ResolvedModel != "claude-haiku-5-5" {
		t.Errorf("resolved = %q", i.ResolvedModel)
	}
}

func TestSkillsInactiveWhenSnapshotMissing(t *testing.T) {
	sink := &infoSink{}
	b := newFakeBackend(t, config.ClaudeConfig{}, map[string]string{envScenario: "fixture", envFixture: fixturePath(t, "skill_invocation.stream.jsonl")},
		WithSkills(func() (string, bool) { return "", false }), WithObserver(sink.observe))
	if _, err := b.Complete(context.Background(), simpleReq); err != nil {
		t.Fatal(err)
	}
	if i := sink.only(t); i.SkillsActive {
		t.Errorf("skills reported active without a snapshot: %+v", i)
	}
}

func TestSkillsCommitFromDir(t *testing.T) {
	tests := []struct{ dir, want string }{
		{"/x/versions/0123456789abcdef0123456789abcdef01234567", "0123456789ab"},
		{"/x/versions/abc1234", "abc1234"},
		{"/x/versions/not-a-sha", ""},
		{"/x/versions/abc", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := skillsCommit(tt.dir); got != tt.want {
			t.Errorf("skillsCommit(%q) = %q, want %q", tt.dir, got, tt.want)
		}
	}
}

func TestSpanEventsAndAttributes(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	snap := filepath.Join(t.TempDir(), "versions", sha)
	if err := os.MkdirAll(snap, 0o700); err != nil {
		t.Fatal(err)
	}
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	b := newFakeBackend(t, config.ClaudeConfig{}, map[string]string{envScenario: "fixture", envFixture: fixturePath(t, "skill_invocation.stream.jsonl")},
		WithTracerProvider(tp), WithSkills(func() (string, bool) { return snap, true }))
	if _, err := b.Stream(context.Background(), simpleReq, func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans = %d", len(spans))
	}
	s := spans[0]
	attrs := map[string]attribute.Value{}
	for _, a := range s.Attributes() {
		attrs[string(a.Key)] = a.Value
	}
	if v, ok := attrs["claude.skills.active"]; !ok || !v.AsBool() {
		t.Errorf("claude.skills.active = %v", v)
	}
	if v := attrs["claude.skills.commit"]; v.AsString() != sha[:12] {
		t.Errorf("claude.skills.commit = %q", v.AsString())
	}
	if v := attrs["claude.skills.invoked"]; !reflect.DeepEqual(v.AsStringSlice(), []string{"summarize"}) {
		t.Errorf("claude.skills.invoked = %v", v.AsStringSlice())
	}
	for _, k := range []string{"claude.queue_wait_ms", "claude.ttft_ms", "claude.num_turns", "claude.model.resolved"} {
		if _, ok := attrs[k]; !ok {
			t.Errorf("missing span attribute %s (have %v)", k, attrs)
		}
	}
	var first int
	for _, e := range s.Events() {
		if e.Name == "first_token" {
			first++
			var has bool
			for _, a := range e.Attributes {
				if a.Key == "claude.elapsed_ms" {
					has = true
				}
			}
			if !has {
				t.Errorf("first_token event lacks elapsed_ms: %v", e.Attributes)
			}
		}
	}
	if first != 1 {
		t.Errorf("first_token events = %d, want exactly 1 (events %v)", first, s.Events())
	}
}

func TestSpanWithoutSkillsReportsInactive(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	b := newFakeBackend(t, config.ClaudeConfig{}, map[string]string{envScenario: "fixture", envFixture: fixturePath(t, "success.stream.jsonl")}, WithTracerProvider(tp))
	if _, err := b.Complete(context.Background(), simpleReq); err != nil {
		t.Fatal(err)
	}
	var active, hasCommit bool
	var found bool
	for _, a := range sr.Ended()[0].Attributes() {
		switch a.Key {
		case "claude.skills.active":
			found, active = true, a.Value.AsBool()
		case "claude.skills.commit", "claude.skills.invoked":
			hasCommit = true
		}
	}
	if !found || active || hasCommit {
		t.Errorf("skills attrs without skills: found=%v active=%v extra=%v", found, active, hasCommit)
	}
}

func TestStreamParserSkillToolUse(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"model":"m","content":[{"type":"tool_use","name":"Skill","input":{"skill":"summarize","args":"secret prompt text"}}]}}`,
		`{"type":"assistant","message":{"model":"m","content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"assistant","message":{"model":"m","content":[{"type":"tool_use","name":"Skill","input":{}}]}}`,
		`{"type":"assistant","message":{"model":"m","content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"assistant","message":{"model":"m","content":"plain string content"}}`,
		`{"type":"user","message":{"role":"user","content":"a string, not blocks"}}`,
		`{"type":"assistant","message":{"model":"m","content":[{"type":"tool_use","name":"Skill","input":{"skill":"core-skills:summarize"}}]}}`,
	}
	p := newStreamParser()
	for _, l := range lines {
		if _, err := p.feed([]byte(l)); err != nil {
			t.Fatalf("feed(%s): %v", l, err)
		}
	}
	if want := []string{"summarize", "core-skills:summarize"}; !reflect.DeepEqual(p.skills, want) {
		t.Errorf("skills = %v, want %v", p.skills, want)
	}
	if strings.Contains(strings.Join(p.skills, ","), "secret") {
		t.Error("tool arguments must never be captured")
	}
}
