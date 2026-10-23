package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/martinezsaweczko/claudeai-api/config"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// ---------------------------------------------------------------------------
// Fake CLI: the test binary re-executes itself as "claude".
// ---------------------------------------------------------------------------

const (
	envHelper   = "GO_WANT_HELPER_PROCESS"
	envScenario = "CLAUDE_HELPER_SCENARIO"
	envFixture  = "CLAUDE_HELPER_FIXTURE"
	envExit     = "CLAUDE_HELPER_EXIT"
	envStderr   = "CLAUDE_HELPER_STDERR"
	envDir      = "CLAUDE_HELPER_DIR"
)

// TestHelperProcess is not a real test: it is the fake claude executable.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(envHelper) != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	dir := os.Getenv(envDir)
	switch os.Getenv(envScenario) {
	case "fixture":
		data, err := os.ReadFile(os.Getenv(envFixture))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(99)
		}
		os.Stdout.Write(data)
	case "echo":
		stdin, _ := io.ReadAll(os.Stdin)
		cwd, _ := os.Getwd()
		env := map[string]string{}
		for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
			if v, ok := os.LookupEnv(k); ok {
				env[k] = v
			}
		}
		payload, _ := json.Marshal(map[string]any{"args": args, "stdin": string(stdin), "env": env, "cwd": cwd})
		line, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": string(payload)})
		os.Stdout.Write(append(line, '\n'))
	case "block":
		// Signal start, wait for the release file, then emit the fixture.
		os.WriteFile(filepath.Join(dir, "started"), nil, 0o600)
		for {
			if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		data, _ := os.ReadFile(os.Getenv(envFixture))
		os.Stdout.Write(data)
	case "hang":
		// Spawn a grandchild, record both pids, then hang forever.
		child := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "--")
		child.Env = append(os.Environ(), envScenario+"=sleep")
		if err := child.Start(); err != nil {
			os.Exit(98)
		}
		os.WriteFile(filepath.Join(dir, "pids"), []byte(fmt.Sprintf("%d %d", os.Getpid(), child.Process.Pid)), 0o600)
		fmt.Println(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}}`)
		time.Sleep(time.Hour)
	case "escape":
		// Start a grandchild in its own session (outside the process group) that
		// inherits stdout and holds it open, then hang.
		child := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "--")
		child.Env = append(os.Environ(), envScenario+"=sleep")
		child.Stdout = os.Stdout
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			os.Exit(98)
		}
		os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		time.Sleep(time.Hour)
	case "sleep":
		time.Sleep(time.Hour)
	case "hugeline":
		os.Stdout.WriteString(strings.Repeat("x", maxLineBytes+10) + "\n")
	default:
		os.Exit(97)
	}
	if code, _ := strconv.Atoi(os.Getenv(envExit)); code != 0 {
		if s := os.Getenv(envStderr); s != "" {
			fmt.Fprintln(os.Stderr, s)
		}
		os.Exit(code)
	}
	os.Exit(0)
}

// newFakeBackend returns a CLIBackend that executes the helper process with the
// given scenario variables.
func newFakeBackend(t *testing.T, cfg config.ClaudeConfig, scenario map[string]string, extra ...Option) *CLIBackend {
	t.Helper()
	if cfg.Bin == "" {
		cfg.Bin = os.Args[0]
	}
	if cfg.Model == "" {
		cfg.Model = "sonnet"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxConcurrency == 0 {
		cfg.MaxConcurrency = 2
	}
	env := append(os.Environ(), envHelper+"=1")
	for k, v := range scenario {
		env = append(env, k+"="+v)
	}
	opts := []Option{
		WithBinArgs("-test.run=^TestHelperProcess$", "--"),
		WithEnv(func() []string { return env }),
		WithWorkDir(t.TempDir()),
	}
	opts = append(opts, extra...)
	return NewCLIBackend(cfg, opts...)
}

func fixturePath(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var simpleReq = Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}

// ---------------------------------------------------------------------------
// End to end tests against the fake CLI
// ---------------------------------------------------------------------------

func TestStreamSuccess(t *testing.T) {
	b := newFakeBackend(t, config.ClaudeConfig{}, map[string]string{envScenario: "fixture", envFixture: fixturePath(t, "success.stream.jsonl")})

	var got []Event
	res, err := b.Stream(context.Background(), simpleReq, func(e Event) error {
		got = append(got, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "OK" || res.Usage.OutputTokens != 4 {
		t.Fatalf("res = %+v", res)
	}
	if len(got) != 2 || got[0].Type != EventTextDelta || got[1].Type != EventResult {
		t.Fatalf("events = %+v", got)
	}

	// Complete is built on Stream.
	res2, err := b.Complete(context.Background(), simpleReq)
	if err != nil || res2.Text != "OK" {
		t.Fatalf("Complete = %+v, %v", res2, err)
	}
}

func TestStreamEmitErrorStopsRun(t *testing.T) {
	b := newFakeBackend(t, config.ClaudeConfig{}, map[string]string{envScenario: "fixture", envFixture: fixturePath(t, "success.stream.jsonl")})
	sentinel := errors.New("client went away")
	_, err := b.Stream(context.Background(), simpleReq, func(Event) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestStreamErrorMapping(t *testing.T) {
	tests := []struct {
		name     string
		scenario map[string]string
		want     error
		contains string
	}{
		{"auth error result", map[string]string{envScenario: "fixture", envFixture: "auth_error.stream.jsonl", envExit: "1"}, ErrAuth, "login"},
		{"rate limited result", map[string]string{envScenario: "fixture", envFixture: "rate_limited.stream.jsonl", envExit: "1"}, ErrRateLimited, "usage limit"},
		{"invalid model result", map[string]string{envScenario: "fixture", envFixture: "invalid_model.stream.jsonl", envExit: "1"}, ErrInvalidModel, "nonexistent-model-xyz"},
		{"truncated stream, exit 0", map[string]string{envScenario: "fixture", envFixture: "truncated.stream.jsonl"}, ErrCLI, "without a result"},
		{"non-zero exit with stderr", map[string]string{envScenario: "fixture", envFixture: "empty.jsonl", envExit: "3", envStderr: "boom: something broke"}, ErrCLI, "something broke"},
		{"auth text on stderr", map[string]string{envScenario: "fixture", envFixture: "empty.jsonl", envExit: "1", envStderr: "Not logged in. Please run /login"}, ErrAuth, "logged in"},
		{"oversized line", map[string]string{envScenario: "hugeline"}, ErrCLI, "line"},
	}
	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := tt.scenario
			switch sc[envFixture] {
			case "empty.jsonl":
				sc[envFixture] = empty
			case "":
			default:
				sc[envFixture] = fixturePath(t, sc[envFixture])
			}
			b := newFakeBackend(t, config.ClaudeConfig{}, sc)
			_, err := b.Complete(context.Background(), simpleReq)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if !containsFold(err.Error(), tt.contains) {
				t.Errorf("err %q does not contain %q", err, tt.contains)
			}
		})
	}
}

func TestBinaryNotFound(t *testing.T) {
	b := NewCLIBackend(config.ClaudeConfig{Bin: "/nonexistent/claude-bin", Model: "sonnet", Timeout: time.Second, MaxConcurrency: 1}, WithWorkDir(t.TempDir()))
	_, err := b.Complete(context.Background(), simpleReq)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestInvalidRequestDoesNotSpawn(t *testing.T) {
	b := NewCLIBackend(config.ClaudeConfig{Bin: "/nonexistent/claude-bin", Model: "sonnet", Timeout: time.Second, MaxConcurrency: 1}, WithWorkDir(t.TempDir()))
	_, err := b.Complete(context.Background(), Request{})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestInvocationShape(t *testing.T) {
	workDir := t.TempDir()
	t.Setenv("ANTHROPIC_API_KEY", "should-not-leak")
	b := newFakeBackend(t,
		config.ClaudeConfig{OAuthToken: "cfg-token"},
		map[string]string{
			envScenario:            "echo",
			"ANTHROPIC_API_KEY":    "sk-ant-api-secret",
			"ANTHROPIC_AUTH_TOKEN": "auth-secret",
		},
		WithWorkDir(workDir),
	)

	req := Request{
		Model:  "opus",
		System: "be brief",
		Messages: []Message{
			{Role: RoleUser, Content: "first"},
			{Role: RoleAssistant, Content: "second"},
			{Role: RoleUser, Content: "third"},
		},
	}
	res, err := b.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Args  []string
		Stdin string
		Env   map[string]string
		Cwd   string
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode echo: %v (%q)", err, res.Text)
	}

	// Prompt travels on stdin, never in argv.
	if !strings.Contains(out.Stdin, "third") || !strings.Contains(out.Stdin, "second") {
		t.Errorf("stdin = %q", out.Stdin)
	}
	for _, a := range out.Args {
		if strings.Contains(a, "third") || strings.Contains(a, "second") {
			t.Errorf("prompt content leaked into argv: %v", out.Args)
		}
	}
	joined := strings.Join(out.Args, " ")
	for _, want := range []string{"--model opus", "--system-prompt be brief", "stream-json"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}

	// Env filtering.
	if _, ok := out.Env["ANTHROPIC_API_KEY"]; ok {
		t.Error("ANTHROPIC_API_KEY must be removed from the child env")
	}
	if _, ok := out.Env["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Error("ANTHROPIC_AUTH_TOKEN must be removed from the child env")
	}
	if out.Env["CLAUDE_CODE_OAUTH_TOKEN"] != "cfg-token" {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN = %q, want cfg-token", out.Env["CLAUDE_CODE_OAUTH_TOKEN"])
	}

	// cwd is the isolated working directory.
	gotCwd, _ := filepath.EvalSymlinks(out.Cwd)
	wantCwd, _ := filepath.EvalSymlinks(workDir)
	if gotCwd != wantCwd {
		t.Errorf("cwd = %q, want %q", gotCwd, wantCwd)
	}
}

func TestDefaultModelAndSystem(t *testing.T) {
	b := newFakeBackend(t, config.ClaudeConfig{Model: "haiku"}, map[string]string{envScenario: "echo"})
	res, err := b.Complete(context.Background(), simpleReq)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, `--model","haiku`) {
		t.Errorf("default model not used: %s", res.Text)
	}
	if !strings.Contains(res.Text, "--system-prompt") {
		t.Errorf("a default system prompt must always be passed: %s", res.Text)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	dir := t.TempDir()
	b := newFakeBackend(t, config.ClaudeConfig{MaxConcurrency: 1}, map[string]string{
		envScenario: "block", envFixture: fixturePath(t, "success.stream.jsonl"), envDir: dir,
	})

	first := make(chan error, 1)
	go func() {
		_, err := b.Complete(context.Background(), simpleReq)
		first <- err
	}()
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "started")); return err == nil })

	// The slot is taken: a second caller waits and gives up when its ctx ends.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := b.Complete(ctx, simpleReq)
	if !errors.Is(err, ErrBusy) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want ErrBusy wrapping DeadlineExceeded", err)
	}

	// Cancelling while waiting returns promptly with context.Canceled.
	ctx2, cancel2 := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := b.Complete(ctx2, simpleReq); done <- err }()
	time.Sleep(30 * time.Millisecond)
	cancel2()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrBusy) {
			t.Fatalf("err = %v, want ErrBusy wrapping Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiting caller did not return after cancel")
	}

	// Releasing the first lets it finish and frees the slot.
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-first; err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := b.Complete(context.Background(), simpleReq); err != nil {
		t.Fatalf("call after release: %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func processGone(pid int) bool {
	// Signal 0 only probes. A zombie still answers, but the group kill plus
	// Wait/reparenting removes it quickly; poll to be tolerant.
	return syscall.Kill(pid, 0) == syscall.ESRCH
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	b := newFakeBackend(t, config.ClaudeConfig{Timeout: 400 * time.Millisecond}, map[string]string{envScenario: "hang", envDir: dir})

	var deltas []string
	start := time.Now()
	_, err := b.Stream(context.Background(), simpleReq, func(e Event) error {
		if e.Type == EventTextDelta {
			deltas = append(deltas, e.Text)
		}
		return nil
	})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("timeout took %v", time.Since(start))
	}
	if len(deltas) != 1 || deltas[0] != "partial" {
		t.Errorf("deltas before timeout = %q", deltas)
	}

	raw, rerr := os.ReadFile(filepath.Join(dir, "pids"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	var parent, child int
	if _, err := fmt.Sscanf(string(raw), "%d %d", &parent, &child); err != nil {
		t.Fatal(err)
	}
	for name, pid := range map[string]int{"child": parent, "grandchild": child} {
		pid := pid
		waitFor(t, func() bool { return processGone(pid) })
		t.Logf("%s pid %d is gone", name, pid)
	}
}

// A descendant that escapes the process group and keeps stdout open must not
// pin Stream (or its concurrency slot) past timeout + WaitDelay.
func TestEscapedDescendantCannotPinStream(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() {
		if raw, err := os.ReadFile(filepath.Join(dir, "pid")); err == nil {
			if pid, err := strconv.Atoi(string(raw)); err == nil {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	b := newFakeBackend(t, config.ClaudeConfig{Timeout: 300 * time.Millisecond, MaxConcurrency: 1},
		map[string]string{envScenario: "escape", envDir: dir})

	done := make(chan error, 1)
	go func() { _, err := b.Complete(context.Background(), simpleReq); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("err = %v, want ErrTimeout", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stream did not return: escaped descendant pinned stdout")
	}

	// The slot must have been released.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := b.acquire(ctx); err != nil {
		t.Fatalf("concurrency slot leaked: %v", err)
	}
	<-b.sem
}

func TestParentCancelReturnsContextError(t *testing.T) {
	dir := t.TempDir()
	b := newFakeBackend(t, config.ClaudeConfig{}, map[string]string{envScenario: "hang", envDir: dir})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "pids")); return err == nil })
		cancel()
	}()
	_, err := b.Complete(ctx, simpleReq)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrTimeout) {
		t.Error("client cancel must not be reported as a timeout")
	}
}

func TestSpanAndObserver(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	var mu sync.Mutex
	var infos []RunInfo

	scn := map[string]string{envScenario: "fixture", envFixture: fixturePath(t, "success.stream.jsonl")}
	b := newFakeBackend(t, config.ClaudeConfig{}, scn,
		WithTracerProvider(tp),
		WithObserver(func(i RunInfo) { mu.Lock(); infos = append(infos, i); mu.Unlock() }),
	)
	if _, err := b.Complete(context.Background(), simpleReq); err != nil {
		t.Fatal(err)
	}

	spans := sr.Ended()
	if len(spans) != 1 || spans[0].Name() != "claude.run" {
		t.Fatalf("spans = %v", spans)
	}
	attrs := map[string]string{}
	for _, a := range spans[0].Attributes() {
		attrs[string(a.Key)] = a.Value.Emit()
	}
	for k, v := range map[string]string{
		"claude.model":         "sonnet",
		"claude.outcome":       "ok",
		"claude.exit_code":     "0",
		"claude.tokens.output": "4",
		"claude.tokens.input":  "2",
	} {
		if attrs[k] != v {
			t.Errorf("attr %s = %q, want %q (all: %v)", k, attrs[k], v, attrs)
		}
	}
	if _, ok := attrs["claude.duration_ms"]; !ok {
		t.Error("missing claude.duration_ms")
	}
	if len(infos) != 1 || infos[0].Outcome != "ok" || infos[0].Usage.OutputTokens != 4 {
		t.Fatalf("observer infos = %+v", infos)
	}

	// Failure: error recorded on the span, outcome reflects the mapped error.
	sr2 := tracetest.NewSpanRecorder()
	tp2 := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr2))
	scn2 := map[string]string{envScenario: "fixture", envFixture: fixturePath(t, "auth_error.stream.jsonl"), envExit: "1"}
	b2 := newFakeBackend(t, config.ClaudeConfig{}, scn2, WithTracerProvider(tp2))
	if _, err := b2.Complete(context.Background(), simpleReq); !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v", err)
	}
	s := sr2.Ended()[0]
	if len(s.Events()) == 0 || s.Events()[0].Name != "exception" {
		t.Errorf("error not recorded on span: %v", s.Events())
	}
	var outcome string
	for _, a := range s.Attributes() {
		if a.Key == "claude.outcome" {
			outcome = a.Value.Emit()
		}
	}
	if outcome != "auth" {
		t.Errorf("outcome = %q, want auth", outcome)
	}
}

// Optional smoke test against the real CLI. Skipped unless explicitly enabled;
// it makes one tiny real request with the local login.
func TestIntegrationRealCLI(t *testing.T) {
	if os.Getenv("CLAUDEAI_INTEGRATION") != "1" {
		t.Skip("set CLAUDEAI_INTEGRATION=1 to run against the real claude CLI")
	}
	b := NewCLIBackend(config.ClaudeConfig{Bin: "claude", Model: "sonnet", Timeout: 2 * time.Minute, MaxConcurrency: 1})
	defer b.Close()
	res, err := b.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "Reply with the single word OK"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToUpper(res.Text), "OK") {
		t.Errorf("unexpected reply %q", res.Text)
	}
}
