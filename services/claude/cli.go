package claude

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/martinezsaweczko/claudeai-api/config"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const (
	tracerName      = "claudeai-api/service"
	maxStderrBytes  = 8 << 10
	scanInitialSize = 64 << 10
)

// RunInfo describes one finished execution. It is delivered to the optional
// observer so metrics (T7) can be added without touching the run logic.
type RunInfo struct {
	Model    string
	Outcome  string // ok, auth, rate_limited, timeout, busy, canceled, not_found, invalid_model, invalid_request, cli_error
	ExitCode int    // -1 when the process did not run to completion
	Duration time.Duration
	Usage    Usage
	CostUSD  float64
	Err      error
}

// Option customizes a CLIBackend.
type Option func(*CLIBackend)

// WithBinArgs prepends fixed arguments to every invocation (used by tests to
// re-execute the test binary as a fake CLI).
func WithBinArgs(args ...string) Option { return func(b *CLIBackend) { b.binArgs = args } }

// WithEnv replaces the source of the base environment (default os.Environ).
// ANTHROPIC_API_KEY and ANTHROPIC_AUTH_TOKEN are still removed from it.
func WithEnv(f func() []string) Option { return func(b *CLIBackend) { b.environ = f } }

// WithWorkDir sets the directory the CLI runs in. It must be empty of project
// files; it is not removed by Close. Default: a private temp directory.
func WithWorkDir(dir string) Option { return func(b *CLIBackend) { b.workDir = dir } }

// WithTracerProvider sets the provider used for the claude.run span.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(b *CLIBackend) {
		if tp != nil {
			b.tracer = tp.Tracer(tracerName)
		}
	}
}

// WithObserver registers a hook called once per finished run.
func WithObserver(f func(RunInfo)) Option { return func(b *CLIBackend) { b.observe = f } }

// WithLogger sets the logger (never receives prompts or secrets).
func WithLogger(l *slog.Logger) Option {
	return func(b *CLIBackend) {
		if l != nil {
			b.log = l
		}
	}
}

// CLIBackend implements Backend by running `claude -p` once per request.
type CLIBackend struct {
	cfg     config.ClaudeConfig
	binArgs []string
	environ func() []string
	tracer  trace.Tracer
	observe func(RunInfo)
	log     *slog.Logger
	sem     chan struct{}

	workDir  string
	dirOnce  sync.Once
	dirErr   error
	ownedDir bool
}

var _ Backend = (*CLIBackend)(nil)

// NewCLIBackend builds the backend from config.
func NewCLIBackend(cfg config.ClaudeConfig, opts ...Option) *CLIBackend {
	conc := max(cfg.MaxConcurrency, 1)
	b := &CLIBackend{
		cfg:     cfg,
		environ: os.Environ,
		tracer:  noop.NewTracerProvider().Tracer(tracerName),
		log:     slog.New(slog.DiscardHandler),
		sem:     make(chan struct{}, conc),
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Close removes the temp working directory created by the backend, if any.
func (b *CLIBackend) Close() error {
	if b.ownedDir && b.workDir != "" {
		return os.RemoveAll(b.workDir)
	}
	return nil
}

// cwd returns the isolated working directory, creating it on first use.
func (b *CLIBackend) cwd() (string, error) {
	b.dirOnce.Do(func() {
		if b.workDir != "" {
			return
		}
		dir, err := os.MkdirTemp("", "claudeai-api-cwd-")
		if err != nil {
			b.dirErr = err
			return
		}
		b.workDir, b.ownedDir = dir, true
	})
	return b.workDir, b.dirErr
}

// Complete implements Backend on top of Stream.
func (b *CLIBackend) Complete(ctx context.Context, req Request) (Result, error) {
	return b.Stream(ctx, req, nil)
}

// Stream implements Backend.
func (b *CLIBackend) Stream(ctx context.Context, req Request, emit func(Event) error) (res Result, err error) {
	model := req.Model
	if model == "" {
		model = b.cfg.Model
	}

	ctx, span := b.tracer.Start(ctx, "claude.run", trace.WithAttributes(attribute.String("claude.model", model)))
	start := time.Now()
	exitCode := -1
	defer func() {
		info := RunInfo{Model: model, Outcome: outcomeOf(err), ExitCode: exitCode, Duration: time.Since(start), Usage: res.Usage, CostUSD: res.CostUSD, Err: err}
		span.SetAttributes(
			attribute.String("claude.outcome", info.Outcome),
			attribute.Int("claude.exit_code", exitCode),
			attribute.Int64("claude.duration_ms", info.Duration.Milliseconds()),
			attribute.Int("claude.tokens.input", res.Usage.InputTokens),
			attribute.Int("claude.tokens.output", res.Usage.OutputTokens),
			attribute.Int("claude.tokens.cache_creation", res.Usage.CacheCreationTokens),
			attribute.Int("claude.tokens.cache_read", res.Usage.CacheReadTokens),
			attribute.Float64("claude.cost_usd", res.CostUSD),
		)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, info.Outcome)
		}
		span.End()
		if b.observe != nil {
			b.observe(info)
		}
	}()

	prompt, err := renderPrompt(req.Messages)
	if err != nil {
		return Result{}, err
	}
	system := req.System
	if strings.TrimSpace(system) == "" {
		system = defaultSystemPrompt
	} else if len(system) > maxSystemArgBytes {
		prompt = system + "\n\n" + prompt
		system = defaultSystemPrompt
	}

	if err := b.acquire(ctx); err != nil {
		return Result{}, err
	}
	defer func() { <-b.sem }()

	runCtx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()

	dir, err := b.cwd()
	if err != nil {
		return Result{}, fmt.Errorf("%w: preparing working directory: %v", ErrCLI, err)
	}

	args := append(append([]string{}, b.binArgs...), buildArgs(model, system)...)
	cmd := exec.CommandContext(runCtx, b.cfg.Bin, args...)
	cmd.Dir = dir
	cmd.Env = filterEnv(b.environ(), b.cfg.OAuthToken)
	cmd.Stdin = strings.NewReader(prompt)
	stderr := &boundedBuffer{max: maxStderrBytes}
	cmd.Stderr = stderr
	configureProcess(cmd)

	// A non-*os.File writer makes os/exec own a copy goroutine that WaitDelay
	// can bound, so Wait returns even if an escaped descendant keeps the OS
	// pipe open. Wait runs concurrently and closes pw to give the scanner EOF.
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return Result{}, fmt.Errorf("%w: %s", ErrNotFound, sanitize(err.Error()))
		}
		return Result{}, fmt.Errorf("%w: starting process: %s", ErrCLI, sanitize(err.Error(), b.cfg.OAuthToken))
	}

	var waitErr error
	waited := make(chan struct{})
	go func() {
		waitErr = cmd.Wait()
		pw.Close()
		close(waited)
	}()

	parser := newStreamParser()
	var emitErr error
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, scanInitialSize), maxLineBytes)
scan:
	for sc.Scan() {
		events, perr := parser.feed(sc.Bytes())
		if perr != nil {
			b.log.Debug("ignoring malformed claude stream line")
			continue
		}
		if emit == nil {
			continue
		}
		for _, e := range events {
			if emitErr = emit(e); emitErr != nil {
				cancel() // consumer is gone: stop the process
				break scan
			}
		}
	}
	scanErr := sc.Err()
	if scanErr != nil {
		cancel()
	}
	pr.Close() // unblock the copy goroutine if we stopped reading early
	<-waited
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	switch {
	case emitErr != nil:
		return Result{}, emitErr
	case parser.hasResult():
		return parser.finish()
	case errors.Is(scanErr, bufio.ErrTooLong):
		return Result{}, fmt.Errorf("%w: reading stream (line limit %d bytes): %s", ErrCLI, maxLineBytes, sanitize(scanErr.Error()))
	case runCtx.Err() != nil:
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return Result{}, fmt.Errorf("%w after %s", ErrTimeout, time.Since(start).Round(time.Millisecond))
		}
		return Result{}, runCtx.Err()
	case scanErr != nil:
		return Result{}, fmt.Errorf("%w: reading stream (line limit %d bytes): %s", ErrCLI, maxLineBytes, sanitize(scanErr.Error()))
	case waitErr != nil:
		return Result{}, b.exitError(waitErr, stderr.String())
	}
	return parser.finish()
}

// exitError maps a non-zero exit without a result line to a typed error using
// the bounded stderr as context.
func (b *CLIBackend) exitError(waitErr error, stderr string) error {
	msg := sanitize(stderr, b.cfg.OAuthToken)
	var exitErr *exec.ExitError
	code := -1
	if errors.As(waitErr, &exitErr) {
		code = exitErr.ExitCode()
	}
	switch {
	case authTextRe.MatchString(stderr):
		return fmt.Errorf("%w: %s", ErrAuth, msg)
	case limitTextRe.MatchString(stderr):
		return fmt.Errorf("%w: %s", ErrRateLimited, msg)
	}
	return fmt.Errorf("%w: exit code %d: %s", ErrCLI, code, msg)
}

// acquire takes a concurrency slot, honouring ctx while waiting.
func (b *CLIBackend) acquire(ctx context.Context) error {
	select {
	case b.sem <- struct{}{}:
		return nil
	default:
	}
	select {
	case b.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ErrBusy, ctx.Err())
	}
}

func outcomeOf(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrBusy):
		return "busy"
	case errors.Is(err, ErrAuth):
		return "auth"
	case errors.Is(err, ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, ErrTimeout):
		return "timeout"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrInvalidModel):
		return "invalid_model"
	case errors.Is(err, ErrInvalidRequest):
		return "invalid_request"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "cli_error"
}

// boundedBuffer keeps the first max bytes written and discards the rest.
type boundedBuffer struct {
	buf []byte
	max int
}

func (w *boundedBuffer) Write(p []byte) (int, error) {
	if room := w.max - len(w.buf); room > 0 {
		w.buf = append(w.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (w *boundedBuffer) String() string { return string(w.buf) }
