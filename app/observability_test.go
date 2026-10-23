package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/martinezsaweczko/claudeai-api/config"
	"github.com/martinezsaweczko/claudeai-api/model"
	"github.com/martinezsaweczko/claudeai-api/o11"
	"github.com/martinezsaweczko/claudeai-api/services/skills"
	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const (
	fixtureSuccess = "../services/claude/testdata/success.stream.jsonl"
	fixtureSkill   = "../services/claude/testdata/skill_invocation.stream.jsonl"
	fixtureAuth    = "../services/claude/testdata/auth_error.stream.jsonl"
)

// obsEnv is the real app wiring (initAndWire) around a real CLIBackend whose
// binary is a shell script replaying a recorded stream, with an in-memory
// span recorder, a Prometheus registry and a JSON log buffer.
type obsEnv struct {
	srv   *muxServer
	spans *tracetest.SpanRecorder
	reg   *prom.Registry
	logs  *bytes.Buffer
	app   *App
}

type obsOpts struct {
	fixture string
	exit    int
	skills  *fakeSyncer
}

func newObsEnv(t *testing.T, o obsOpts) *obsEnv {
	t.Helper()
	script := filepath.Join(t.TempDir(), "claude")
	body := fmt.Sprintf("#!/bin/sh\ncat >/dev/null\ncat '%s'\nexit %d\n", mustAbs(t, o.fixture), o.exit)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp) // otelhttp (TracingMiddleware) reads the global provider
	t.Cleanup(func() { otel.SetTracerProvider(prev); _ = tp.Shutdown(context.Background()) })

	reg := prom.NewRegistry()
	exp, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		t.Fatal(err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	logs := &bytes.Buffer{}
	log := slog.New(o11.NewTraceLogHandler(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	cfg := &config.Config{Claude: config.ClaudeConfig{
		Bin: script, Model: "sonnet", Models: []string{"sonnet", "opus"},
		Timeout: time.Minute, MaxConcurrency: 2,
	}}
	srv := newMuxServer()
	a := NewApp(cfg).
		WithServer(srv).
		WithLog(log).
		WithObservability(&o11.ObservabilityInst{Trace: tp, MeterProvider: mp}).
		WithBuildInfo(model.BuildInfo{Version: "test"}).
		WithBasePath("/api/v1")
	a.lookupEnv = func(string) (string, bool) { return "", false }
	if o.skills != nil {
		a.skills = o.skills
	}
	if err := a.initAndWire(); err != nil {
		t.Fatalf("initAndWire: %v", err)
	}
	t.Cleanup(func() { _ = a.closeBackend() })
	return &obsEnv{srv: srv, spans: sr, reg: reg, logs: logs, app: a}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatal(err)
	}
	return abs
}

func (e *obsEnv) scrape(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(promhttp.HandlerFor(e.reg, promhttp.HandlerOpts{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// sample returns the value of the first exposition line of name whose labels
// include every wanted `k="v"` pair.
func sample(text, name string, labels ...string) (string, bool) {
	for _, l := range strings.Split(text, "\n") {
		if !strings.HasPrefix(l, name+"{") && !strings.HasPrefix(l, name+" ") {
			continue
		}
		ok := true
		for _, want := range labels {
			if !strings.Contains(l, want) {
				ok = false
				break
			}
		}
		if ok {
			return l[strings.LastIndex(l, " ")+1:], true
		}
	}
	return "", false
}

func (e *obsEnv) wantSample(t *testing.T, text, name, value string, labels ...string) {
	t.Helper()
	if got, ok := sample(text, name, labels...); !ok || got != value {
		var lines []string
		for _, l := range strings.Split(text, "\n") {
			if strings.HasPrefix(l, name) {
				lines = append(lines, l)
			}
		}
		t.Errorf("%s%v = %q (found=%v), want %q; lines:\n%s", name, labels, got, ok, value, strings.Join(lines, "\n"))
	}
}

func skillSnapshot(t *testing.T) (*fakeSyncer, string) {
	t.Helper()
	const sha = "0123456789abcdef0123456789abcdef01234567"
	dir := filepath.Join(t.TempDir(), "versions", sha)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &fakeSyncer{have: true, snap: skills.Snapshot{
		Commit: sha, Ref: "main", Dir: dir,
		Skills: []skills.SkillInfo{{Name: "summarize"}, {Name: "translate"}},
	}}, sha[:12]
}

func chatReq(model string, stream bool) string {
	return fmt.Sprintf(`{"model":%q,"stream":%v,"messages":[{"role":"user","content":"SECRET PROMPT TEXT"}]}`, model, stream)
}

func endedByName(t *testing.T, sr *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == name {
			if found != nil {
				t.Fatalf("more than one %q span", name)
			}
			found = s
		}
	}
	if found == nil {
		var names []string
		for _, s := range sr.Ended() {
			names = append(names, s.Name())
		}
		t.Fatalf("no %q span among %v", name, names)
	}
	return found
}

func TestChatMetricsThroughRealWiring(t *testing.T) {
	t.Run("success uses the resolved model", func(t *testing.T) {
		e := newObsEnv(t, obsOpts{fixture: fixtureSuccess})
		if rec := do(e.srv, "POST", "/v1/chat/completions", "", chatReq("sonnet", false)); rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		text := e.scrape(t)
		m := `model="claude-sonnet-5-5"`
		e.wantSample(t, text, "claude_runs_total", "1", m, `outcome="success"`)
		e.wantSample(t, text, "claude_run_duration_seconds_count", "1", m)
		e.wantSample(t, text, "claude_queue_wait_seconds_count", "1", m)
		e.wantSample(t, text, "claude_time_to_first_token_seconds_count", "1", m)
		e.wantSample(t, text, "claude_tokens_total", "2", m, `type="input"`)
		e.wantSample(t, text, "claude_tokens_total", "4", m, `type="output"`)
		e.wantSample(t, text, "claude_tokens_total", "868", m, `type="cache_creation"`)
		e.wantSample(t, text, "claude_runs_in_flight", "0")
		if v, ok := sample(text, "claude_cost_usd_total", m); !ok || v == "0" {
			t.Errorf("claude_cost_usd_total = %q (found=%v)", v, ok)
		}
		if _, ok := sample(text, "claude_tokens_total", m, `type="cache_read"`); ok {
			t.Error("zero-valued token types should not create series")
		}
		if strings.Contains(text, "SECRET PROMPT TEXT") {
			t.Error("prompt text leaked into metrics")
		}
	})

	t.Run("unknown requested model on a failed run is other", func(t *testing.T) {
		e := newObsEnv(t, obsOpts{fixture: fixtureAuth, exit: 1})
		do(e.srv, "POST", "/v1/chat/completions", "", chatReq("gpt-4o-INJECT-1", false))
		do(e.srv, "POST", "/v1/chat/completions", "", chatReq("gpt-4o-INJECT-2", false))
		do(e.srv, "POST", "/v1/chat/completions", "", chatReq("opus", false))
		text := e.scrape(t)
		e.wantSample(t, text, "claude_runs_total", "2", `model="other"`, `outcome="auth"`)
		e.wantSample(t, text, "claude_runs_total", "1", `model="opus"`, `outcome="auth"`)
		if strings.Contains(text, "INJECT") {
			t.Errorf("client-controlled model leaked into a label:\n%s", text)
		}
		// A run that failed before any text has no first token.
		if _, ok := sample(text, "claude_time_to_first_token_seconds_count", `model="other"`); ok {
			t.Error("a run without text must not record time to first token")
		}
	})

	t.Run("skill invocation counted for loaded skills only", func(t *testing.T) {
		syncer, _ := skillSnapshot(t)
		e := newObsEnv(t, obsOpts{fixture: fixtureSkill, skills: syncer})
		do(e.srv, "POST", "/v1/chat/completions", "", chatReq("haiku", false))
		text := e.scrape(t)
		e.wantSample(t, text, "claude_skill_invocations_total", "1", `skill="summarize"`)

		// The same recorded run against a snapshot without that skill is "other".
		syncer2, _ := skillSnapshot(t)
		syncer2.snap.Skills = []skills.SkillInfo{{Name: "translate"}}
		e2 := newObsEnv(t, obsOpts{fixture: fixtureSkill, skills: syncer2})
		do(e2.srv, "POST", "/v1/chat/completions", "", chatReq("haiku", false))
		e2.wantSample(t, e2.scrape(t), "claude_skill_invocations_total", "1", `skill="other"`)
	})

	t.Run("skills gauges follow the syncer", func(t *testing.T) {
		syncer, commit := skillSnapshot(t)
		e := newObsEnv(t, obsOpts{fixture: fixtureSuccess, skills: syncer})
		text := e.scrape(t)
		e.wantSample(t, text, "skills_loaded", "2")
		e.wantSample(t, text, "skills_info", "1", `commit="`+commit[:12]+`"`, `ref="main"`)
		e.wantSample(t, text, "skills_sync_consecutive_failures", "0")
	})

	t.Run("no skills instruments when skills are disabled", func(t *testing.T) {
		e := newObsEnv(t, obsOpts{fixture: fixtureSuccess})
		if text := e.scrape(t); strings.Contains(text, "skills_") {
			t.Errorf("skills metrics exposed while disabled:\n%s", text)
		}
	})
}

func TestSkillsSyncObserverMetrics(t *testing.T) {
	e := newObsEnv(t, obsOpts{fixture: fixtureSuccess})
	obs := newSkillsObserver(mustSkillsMetrics(t, e))
	obs(skills.SyncInfo{Outcome: skills.OutcomeUpdated, Duration: time.Second})
	obs(skills.SyncInfo{Outcome: skills.OutcomeUnchanged, Duration: time.Second})
	obs(skills.SyncInfo{Outcome: skills.OutcomeUnchanged, Duration: time.Second})
	obs(skills.SyncInfo{Outcome: skills.OutcomeError, Duration: time.Second})
	obs(skills.SyncInfo{Outcome: "something-new", Duration: time.Second})
	text := e.scrape(t)
	e.wantSample(t, text, "skills_sync_total", "1", `outcome="success"`)
	e.wantSample(t, text, "skills_sync_total", "2", `outcome="noop"`)
	e.wantSample(t, text, "skills_sync_total", "2", `outcome="error"`)
	e.wantSample(t, text, "skills_sync_duration_seconds_count", "5")
}

func mustSkillsMetrics(t *testing.T, e *obsEnv) *o11.SkillsMetrics {
	t.Helper()
	// A second meter provider on the same registry would clash; reuse the app's.
	m, err := o11.NewSkillsMetrics(e.app.observability.MeterProvider)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTraceShapeNonStreaming(t *testing.T) {
	syncer, commit := skillSnapshot(t)
	e := newObsEnv(t, obsOpts{fixture: fixtureSkill, skills: syncer})
	if rec := do(e.srv, "POST", "/v1/chat/completions", "", chatReq("haiku", false)); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	assertRequestTrace(t, e, commit)
}

func TestTraceShapeStreaming(t *testing.T) {
	syncer, commit := skillSnapshot(t)
	e := newObsEnv(t, obsOpts{fixture: fixtureSkill, skills: syncer})
	rec := do(e.srv, "POST", "/v1/chat/completions", "", chatReq("haiku", true))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	assertRequestTrace(t, e, commit)
}

func assertRequestTrace(t *testing.T, e *obsEnv, commit string) {
	t.Helper()
	server := endedByName(t, e.spans, "POST /v1/chat/completions")
	run := endedByName(t, e.spans, "claude.run")
	if run.SpanContext().TraceID() != server.SpanContext().TraceID() {
		t.Fatalf("claude.run trace %s != server trace %s", run.SpanContext().TraceID(), server.SpanContext().TraceID())
	}
	if run.Parent().SpanID() != server.SpanContext().SpanID() {
		t.Errorf("claude.run parent = %s, want the server span %s", run.Parent().SpanID(), server.SpanContext().SpanID())
	}
	if run.SpanKind() != oteltrace.SpanKindInternal || server.SpanKind() != oteltrace.SpanKindServer {
		t.Errorf("kinds: run=%v server=%v", run.SpanKind(), server.SpanKind())
	}
	attrs := map[string]string{}
	for _, a := range run.Attributes() {
		attrs[string(a.Key)] = a.Value.Emit()
	}
	want := map[string]string{
		"claude.skills.active":     "true",
		"claude.skills.commit":     commit,
		"claude.skills.invoked":    `["summarize"]`,
		"claude.outcome":           "success",
		"claude.model":             "haiku",
		"claude.model.resolved":    "claude-haiku-5-5",
		"claude.num_turns":         "3",
		"claude.tokens.output":     "345",
		"claude.tokens.input":      "4",
		"claude.queue_wait_ms":     attrs["claude.queue_wait_ms"],
		"claude.ttft_ms":           attrs["claude.ttft_ms"],
		"claude.tokens.cache_read": "4791",
	}
	for k, v := range want {
		if got, ok := attrs[k]; !ok || got != v {
			t.Errorf("attr %s = %q (found=%v), want %q; all: %v", k, got, ok, v, attrs)
		}
	}
	var firstTokens int
	for _, ev := range run.Events() {
		if ev.Name == "first_token" {
			firstTokens++
		}
	}
	if firstTokens != 1 {
		t.Errorf("first_token events = %d, want 1", firstTokens)
	}
	for _, a := range run.Attributes() {
		if strings.Contains(a.Value.Emit(), "SECRET PROMPT TEXT") {
			t.Errorf("prompt text leaked into span attribute %s", a.Key)
		}
	}
}

func TestLogsCarryTraceIDsOnTheRequestPath(t *testing.T) {
	e := newObsEnv(t, obsOpts{fixture: fixtureAuth, exit: 1})
	do(e.srv, "POST", "/v1/chat/completions", "", chatReq("sonnet", false))

	server := endedByName(t, e.spans, "POST /v1/chat/completions")
	wantTrace := server.SpanContext().TraceID().String()

	var sawRequestLog, sawFailureLog bool
	for _, line := range strings.Split(strings.TrimSpace(e.logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		msg, _ := rec["msg"].(string)
		switch msg {
		case "HTTP request":
			sawRequestLog = true
		case "Chat completion failed":
			sawFailureLog = true
		default:
			continue
		}
		if rec["trace_id"] != wantTrace || rec["span_id"] == nil {
			t.Errorf("%q log lacks request trace correlation: trace_id=%v span_id=%v (want trace %s)", msg, rec["trace_id"], rec["span_id"], wantTrace)
		}
	}
	if !sawRequestLog || !sawFailureLog {
		t.Fatalf("expected request and failure logs, got:\n%s", e.logs.String())
	}
	if strings.Contains(e.logs.String(), "SECRET PROMPT TEXT") {
		t.Error("prompt text leaked into logs")
	}
}

func TestSkillsRefreshLogCarriesTraceID(t *testing.T) {
	syncer, _ := skillSnapshot(t)
	e := newObsEnv(t, obsOpts{fixture: fixtureSuccess, skills: syncer})
	if rec := do(e.srv, "POST", "/api/v1/skills/refresh", "", ""); rec.Code != 202 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(e.logs.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == "Skills refresh requested" {
			found = true
			if rec["trace_id"] == nil {
				t.Errorf("refresh log lacks trace_id: %s", line)
			}
		}
	}
	if !found {
		t.Fatalf("refresh log not found:\n%s", e.logs.String())
	}
}

func TestMetricsEndpointNeverBreaksWithoutSkills(t *testing.T) {
	// A wired app with the plain stub backend (no CLIBackend) must not need an
	// observer or in-flight source.
	srv, be := wiredServer(t, nil)
	if rec := do(srv, "POST", "/v1/chat/completions", "", chatBody); rec.Code != 200 || be.calls != 1 {
		t.Fatalf("status %d calls %d", rec.Code, be.calls)
	}
}
