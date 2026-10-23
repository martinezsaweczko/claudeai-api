package skills

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/martinezsaweczko/claudeai-api/config"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const tracerName = "claudeai-api/service"

// Sync outcomes reported to observers and spans.
const (
	OutcomeUpdated   = "updated"   // a new version was materialized
	OutcomeUnchanged = "unchanged" // the resolved commit is already current
	OutcomeError     = "error"
)

// Typed errors callers can test with errors.Is.
var (
	ErrDisabled    = errors.New("skills are disabled (no -skills-repo)")
	ErrGitNotFound = errors.New("git binary not found")
	ErrRefNotFound = errors.New("skills ref not found in repository")
)

var (
	fullSHARe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	hexRe     = regexp.MustCompile(`^[0-9a-f]+$`)
)

// SkillInfo describes one validated skill.
type SkillInfo struct {
	Name        string
	Plugin      string // marketplace plugin that provides it; empty for a root skills/ layout
	Description string
}

// Skipped records a path that was ignored and why.
type Skipped struct {
	Path   string
	Reason string
}

// Snapshot is one immutable, validated version of the skills repository.
// Dir is the directory to use as the Claude CLI working directory: it holds
// only .claude/skills/<name>/ and is read-only. A Snapshot must be treated as
// read-only by callers (its slices are shared between readers).
type Snapshot struct {
	Commit   string
	Ref      string
	Dir      string
	Skills   []SkillInfo
	Skipped  []Skipped
	SyncedAt time.Time
}

// Status is a point-in-time view of the syncer for health endpoints and metrics.
type Status struct {
	LastAttempt         time.Time
	LastSuccess         time.Time
	LastError           string // sanitized
	ConsecutiveFailures int
	Commit              string
	Skills              int
}

// SyncInfo describes one finished sync for the optional observer (T7 metrics).
type SyncInfo struct {
	Outcome  string
	Commit   string
	Skills   int
	Skipped  int
	Duration time.Duration
	Err      error
}

// Option customizes a Syncer.
type Option func(*Syncer)

// WithLogger sets the logger (never receives key material).
func WithLogger(l *slog.Logger) Option {
	return func(s *Syncer) {
		if l != nil {
			s.log = l
		}
	}
}

// WithTracerProvider sets the provider used for the skills.sync span.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(s *Syncer) {
		if tp != nil {
			s.tracer = tp.Tracer(tracerName)
		}
	}
}

// WithObserver registers a hook called once per finished sync.
func WithObserver(f func(SyncInfo)) Option { return func(s *Syncer) { s.observe = f } }

// WithGit overrides the git binary (tests).
func WithGit(bin string) Option { return func(s *Syncer) { s.gitBin = bin } }

// WithInterval overrides the periodic refresh interval, bypassing the 30s
// production minimum enforced by the config (tests). 0 syncs once.
func WithInterval(d time.Duration) Option { return func(s *Syncer) { s.interval = d } }

type flight struct {
	done chan struct{}
	err  error
}

// Syncer keeps a local, validated, versioned copy of the skills repository.
type Syncer struct {
	cfg      config.SkillsConfig
	stateDir string
	repoDir  string
	gitBin   string
	interval time.Duration
	log      *slog.Logger
	tracer   trace.Tracer
	observe  func(SyncInfo)
	runner   *gitRunner

	current atomic.Pointer[Snapshot]
	trigger chan struct{}

	mu     sync.Mutex // guards flight and status
	flight *flight
	status Status
}

// New validates cfg and builds a Syncer. It does not touch the network or
// the state directory.
func New(cfg config.SkillsConfig, opts ...Option) (*Syncer, error) {
	if !cfg.Enabled() {
		return nil, ErrDisabled
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid skills configuration: %w", err)
	}
	s := &Syncer{
		cfg:      cfg,
		interval: cfg.Refresh,
		gitBin:   "git",
		log:      slog.New(slog.DiscardHandler),
		tracer:   noop.NewTracerProvider().Tracer(tracerName),
		trigger:  make(chan struct{}, 1),
	}
	for _, o := range opts {
		o(s)
	}
	bin, err := exec.LookPath(s.gitBin)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrGitNotFound, sanitize(err.Error()))
	}
	s.gitBin = bin

	stateDir, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("resolving skills directory: %w", err)
	}
	s.stateDir = stateDir
	s.repoDir = filepath.Join(stateDir, "repo")

	for what, p := range map[string]string{"ssh key": cfg.SSHKey, "known hosts": cfg.KnownHosts} {
		if p == "" {
			continue
		}
		st, serr := os.Stat(p)
		if serr != nil || st.IsDir() {
			return nil, fmt.Errorf("skills %s file is not readable", what)
		}
	}
	var sshCmd string
	if isSSHRepo(cfg.Repo) {
		sshCmd = buildSSHCommand(cfg.SSHKey, cfg.KnownHosts)
	}
	s.runner = &gitRunner{
		bin:      bin,
		env:      gitEnvironment(os.Environ(), sshCmd),
		workDir:  stateDir,
		redacted: []string{cfg.SSHKey},
	}
	return s, nil
}

// Current returns the latest good snapshot, if any sync has succeeded. It is
// safe for concurrent use and never blocks.
func (s *Syncer) Current() (Snapshot, bool) {
	p := s.current.Load()
	if p == nil {
		return Snapshot{}, false
	}
	return *p, true
}

// Status returns the syncer's health counters.
func (s *Syncer) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Trigger requests a sync from Run (for example from a webhook). It never
// blocks; requests made while one is pending are coalesced.
func (s *Syncer) Trigger() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// Run syncs once, then keeps syncing on the refresh interval (with jitter) and
// on Trigger requests until ctx is done. Failures are logged and the last good
// snapshot is kept; the loop never panics and never blocks readers. With a zero
// interval it syncs only at startup (retrying with backoff until the first
// success) and on Trigger.
func (s *Syncer) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-s.trigger:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		s.syncLogged(ctx)
		if d, ok := s.nextWait(); ok {
			timer.Reset(d)
		}
	}
}

func (s *Syncer) syncLogged(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("skills sync panicked", "panic", fmt.Sprint(r))
		}
	}()
	if err := s.Sync(ctx); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("skills sync failed; keeping last good snapshot", "error", sanitize(err.Error(), s.cfg.SSHKey), "consecutive_failures", s.Status().ConsecutiveFailures)
		}
	}
}

// retryBase is the first retry delay after a failed sync; it doubles up to 64x.
const retryBase = 15 * time.Second

// nextWait is the delay before the next scheduled sync, with +-10% jitter.
// ok is false when nothing is scheduled (refresh 0 and the last sync succeeded).
func (s *Syncer) nextWait() (d time.Duration, ok bool) {
	base := s.interval
	if failures := s.Status().ConsecutiveFailures; failures > 0 {
		if backoff := retryBase << min(failures-1, 6); base == 0 || backoff < base {
			base = backoff
		}
	}
	if base == 0 {
		return 0, false
	}
	jitter := time.Duration(float64(base) * 0.1 * (rand.Float64()*2 - 1))
	return base + jitter, true
}

// Sync performs one synchronous sync. Concurrent calls share a single run.
// It is idempotent: when the resolved commit is already current no new version
// is created.
func (s *Syncer) Sync(ctx context.Context) error {
	s.mu.Lock()
	if f := s.flight; f != nil {
		s.mu.Unlock()
		select {
		case <-f.done:
			return f.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{})}
	s.flight = f
	s.mu.Unlock()

	f.err = s.syncOnce(ctx)

	s.mu.Lock()
	s.flight = nil
	s.mu.Unlock()
	close(f.done)
	return f.err
}

func (s *Syncer) syncOnce(ctx context.Context) (err error) {
	ctx, span := s.tracer.Start(ctx, "skills.sync", trace.WithAttributes(
		attribute.String("skills.repo", s.cfg.Repo),
		attribute.String("skills.ref", s.cfg.Ref),
	))
	start := time.Now()
	info := SyncInfo{Outcome: OutcomeError}
	attempt := start
	defer func() {
		info.Duration = time.Since(start)
		info.Err = err
		if err != nil {
			info.Outcome = OutcomeError
			err = fmt.Errorf("syncing skills: %w", err)
			info.Err = err
			span.RecordError(err)
			span.SetStatus(codes.Error, "sync failed")
		}
		span.SetAttributes(
			attribute.String("skills.commit", info.Commit),
			attribute.String("skills.outcome", info.Outcome),
			attribute.Int("skills.count", info.Skills),
			attribute.Int("skills.skipped", info.Skipped),
		)
		span.End()

		s.mu.Lock()
		s.status.LastAttempt = attempt
		if err != nil {
			s.status.ConsecutiveFailures++
			s.status.LastError = sanitize(err.Error(), s.cfg.SSHKey)
		} else {
			s.status.ConsecutiveFailures = 0
			s.status.LastError = ""
			s.status.LastSuccess = time.Now()
			s.status.Commit, s.status.Skills = info.Commit, info.Skills
		}
		s.mu.Unlock()
		if s.observe != nil {
			s.observe(info)
		}
	}()

	if err := os.MkdirAll(s.stateDir, 0o700); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}
	gctx, cancel := context.WithTimeout(ctx, s.cfg.SyncTimeout)
	defer cancel()

	if err := s.updateRepo(gctx); err != nil {
		return err
	}
	sha, err := s.resolveRef(gctx)
	if err != nil {
		return err
	}
	info.Commit = sha

	if cur := s.current.Load(); cur != nil && cur.Commit == sha {
		if _, serr := os.Stat(cur.Dir); serr == nil {
			info.Outcome, info.Skills, info.Skipped = OutcomeUnchanged, len(cur.Skills), len(cur.Skipped)
			return nil
		}
	}

	out, err := s.runner.run(gctx, nil, maxStdoutBytes, "ls-tree", "-r", "-l", "-z", "--full-tree", sha)
	if err != nil {
		return err
	}
	entries, err := parseTree(out)
	if err != nil {
		return err
	}
	disc, err := discover(gctx, entries, s.readBlobs)
	if err != nil {
		return err
	}
	dir, err := materialize(s.stateDir, sha, disc.Skills)
	if err != nil {
		return err
	}

	snap := &Snapshot{
		Commit:   sha,
		Ref:      s.cfg.Ref,
		Dir:      dir,
		Skipped:  disc.Skipped,
		SyncedAt: time.Now(),
	}
	for _, sk := range disc.Skills {
		snap.Skills = append(snap.Skills, sk.Info)
	}
	s.current.Store(snap)
	info.Outcome, info.Skills, info.Skipped = OutcomeUpdated, len(snap.Skills), len(snap.Skipped)
	s.log.Info("skills synced", "commit", sha, "ref", s.cfg.Ref, "skills", len(snap.Skills), "skipped", len(snap.Skipped))
	for _, sk := range snap.Skipped {
		s.log.Warn("skill skipped", "path", sk.Path, "reason", sk.Reason)
	}
	if perr := prune(s.stateDir, dir); perr != nil {
		s.log.Warn("pruning old skill versions failed", "error", sanitize(perr.Error()))
	}
	return nil
}

// updateRepo clones the bare cache on first use and fetches heads and tags.
func (s *Syncer) updateRepo(ctx context.Context) error {
	cloned := false
	if _, err := os.Stat(filepath.Join(s.repoDir, "HEAD")); err != nil {
		_ = removeAll(s.repoDir) // discard a partial clone
		s.runner.gitDir = ""
		if _, err := s.runner.run(ctx, nil, 1<<20, "clone", "--bare", "--quiet", "--", s.cfg.Repo, s.repoDir); err != nil {
			_ = removeAll(s.repoDir)
			return err
		}
		cloned = true
	}
	s.runner.gitDir = s.repoDir
	if _, err := s.runner.run(ctx, nil, 1<<20, "config", "remote.origin.url", s.cfg.Repo); err != nil {
		return err
	}
	if cloned {
		return nil // a fresh bare clone already has every head and tag
	}
	_, err := s.runner.run(ctx, nil, 1<<20, "fetch", "--quiet", "--prune", "--no-tags", "origin",
		"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
	return err
}

// resolveRef turns the configured ref into a full commit sha. A full sha is
// used as is after verifying that the commit exists; names are looked up as
// branch first, then tag.
func (s *Syncer) resolveRef(ctx context.Context) (string, error) {
	ref := s.cfg.Ref
	candidates := []string{"refs/heads/" + ref, "refs/tags/" + ref}
	if fullSHARe.MatchString(ref) {
		candidates = []string{ref}
	}
	for _, c := range candidates {
		out, err := s.runner.run(ctx, nil, 1<<10, "rev-parse", "--verify", "--quiet", c+"^{commit}")
		if err != nil {
			var ge *gitError
			if errors.As(err, &ge) {
				continue // not found under this name
			}
			return "", err
		}
		sha := strings.TrimSpace(string(out))
		if fullSHARe.MatchString(sha) && hexRe.MatchString(sha) {
			return sha, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrRefNotFound, ref)
}

// readBlobs reads blobs through one `git cat-file --batch` process.
func (s *Syncer) readBlobs(ctx context.Context, oids []string) (map[string][]byte, error) {
	unique := make([]string, 0, len(oids))
	seen := map[string]bool{}
	for _, o := range oids {
		if !hexRe.MatchString(o) {
			return nil, fmt.Errorf("invalid object id")
		}
		if !seen[o] {
			seen[o] = true
			unique = append(unique, o)
		}
	}
	out, err := s.runner.run(ctx, []byte(strings.Join(unique, "\n")+"\n"), maxSkillBytes+maxFileBytes+(1<<20), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	blobs := make(map[string][]byte, len(unique))
	rest := out
	for _, want := range unique {
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			return nil, fmt.Errorf("truncated cat-file output")
		}
		header := strings.Fields(string(rest[:nl]))
		rest = rest[nl+1:]
		if len(header) != 3 || header[0] != want || header[1] != "blob" {
			return nil, fmt.Errorf("unexpected object %q", sanitize(string(bytes.TrimSpace([]byte(strings.Join(header, " "))))))
		}
		size, err := strconv.Atoi(header[2])
		if err != nil || size < 0 || len(rest) < size+1 {
			return nil, fmt.Errorf("malformed cat-file output")
		}
		blobs[want] = append([]byte(nil), rest[:size]...)
		rest = rest[size+1:]
	}
	return blobs, nil
}
