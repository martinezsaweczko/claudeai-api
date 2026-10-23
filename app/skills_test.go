package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/martinezsaweczko/claudeai-api/config"
	"github.com/martinezsaweczko/claudeai-api/model"
	"github.com/martinezsaweczko/claudeai-api/o11"
	"github.com/martinezsaweczko/claudeai-api/services/skills"
	"go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type fakeSyncer struct {
	syncErr   error
	syncDelay time.Duration
	snap      skills.Snapshot
	have      bool
	syncs     atomic.Int32
	running   atomic.Bool
	ignoreCtx bool
	triggers  atomic.Int32
}

func (f *fakeSyncer) Current() (skills.Snapshot, bool) { return f.snap, f.have }
func (f *fakeSyncer) Status() skills.Status            { return skills.Status{} }
func (f *fakeSyncer) Trigger()                         { f.triggers.Add(1) }
func (f *fakeSyncer) Sync(ctx context.Context) error {
	f.syncs.Add(1)
	if f.syncDelay > 0 {
		select {
		case <-time.After(f.syncDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.syncErr
}
func (f *fakeSyncer) Run(ctx context.Context) {
	f.running.Store(true)
	defer f.running.Store(false)
	if f.ignoreCtx {
		select {} // simulates a stuck loop
	}
	<-ctx.Done()
}

type closerBackend struct {
	stubBackend
	closed atomic.Int32
}

func (b *closerBackend) Close() error { b.closed.Add(1); return nil }

func newTestApp(t *testing.T, cfg *config.Config) *App {
	t.Helper()
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	a := NewApp(cfg).
		WithPublicServer(newMuxServer()).
		WithInternalServer(newMuxServer()).
		WithLog(slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithObservability(&o11.ObservabilityInst{Trace: tp, MeterProvider: noop.NewMeterProvider()}).
		WithBuildInfo(model.BuildInfo{Version: "test"}).
		WithBasePath("/api/v1")
	a.lookupEnv = func(string) (string, bool) { return "", false }
	return a
}

func claudeCfg() config.ClaudeConfig {
	return config.ClaudeConfig{Bin: "claude", Model: "sonnet", Models: []string{"sonnet"}, Timeout: time.Minute, MaxConcurrency: 1}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSkillsProviderAdapter(t *testing.T) {
	tests := []struct {
		name   string
		snap   skills.Snapshot
		have   bool
		wantOK bool
	}{
		{"no snapshot", skills.Snapshot{}, false, false},
		{"snapshot without skills", skills.Snapshot{Dir: "/d"}, true, false},
		{"snapshot without dir", skills.Snapshot{Skills: []skills.SkillInfo{{Name: "a"}}}, true, false},
		{"snapshot with skills", skills.Snapshot{Dir: "/d", Skills: []skills.SkillInfo{{Name: "a"}}}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, ok := skillsProvider(&fakeSyncer{snap: tt.snap, have: tt.have})()
			if ok != tt.wantOK || (ok && dir != "/d") || (!ok && dir != "") {
				t.Fatalf("provider = %q, %v", dir, ok)
			}
		})
	}
}

func TestSkillsDisabledByDefault(t *testing.T) {
	a := newTestApp(t, &config.Config{Claude: claudeCfg()})
	a.claude = &stubBackend{}
	if err := a.initAndWire(); err != nil {
		t.Fatal(err)
	}
	if a.skills != nil {
		t.Fatal("skills syncer must not exist when -skills-repo is empty")
	}
	internal := a.internalServer.(*muxServer)
	if rec := do(internal, "GET", "/api/v1/skills", "", ""); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"enabled":false}` {
		t.Errorf("GET = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(internal, "POST", "/api/v1/skills/refresh", "", ""); rec.Code != 404 {
		t.Errorf("POST refresh = %d, want 404", rec.Code)
	}
	if rec := do(a.publicServer.(*muxServer), "GET", "/api/v1/skills", "", ""); rec.Code != 404 {
		t.Errorf("skills on the public server = %d, want 404", rec.Code)
	}
}

func TestSkillsMisconfigurationFailsFast(t *testing.T) {
	cfg := &config.Config{Claude: claudeCfg(), Skills: config.SkillsConfig{
		Repo: "git@github.com:o/r.git", Ref: "main", Dir: t.TempDir(), Refresh: time.Minute, SyncTimeout: time.Minute,
		SSHKey: filepath.Join(t.TempDir(), "missing_key"),
	}}
	a := newTestApp(t, cfg)
	a.claude = &stubBackend{}
	err := a.initAndWire()
	if err == nil || !strings.Contains(err.Error(), "skills") {
		t.Fatalf("initAndWire err = %v, want a skills startup error", err)
	}
	if strings.Contains(err.Error(), "missing_key") {
		t.Errorf("error leaks the key path: %v", err)
	}
}

func TestStartSkillsInitialSyncFailureOnlyWarns(t *testing.T) {
	f := &fakeSyncer{syncErr: errors.New("remote down")}
	a := newTestApp(t, &config.Config{Claude: claudeCfg()})
	a.skills = f
	a.startSkills()
	waitFor(t, "run loop", f.running.Load)
	if f.syncs.Load() != 1 {
		t.Errorf("initial syncs = %d, want 1", f.syncs.Load())
	}
	if err := a.stopSkills(); err != nil {
		t.Fatal(err)
	}
	if f.running.Load() {
		t.Error("run loop still running after stopSkills")
	}
}

func TestStartSkillsInitialSyncIsBounded(t *testing.T) {
	f := &fakeSyncer{syncDelay: time.Hour}
	a := newTestApp(t, &config.Config{Claude: claudeCfg()})
	a.skills = f
	a.initialSkillsTimeout = 50 * time.Millisecond
	start := time.Now()
	a.startSkills()
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("startSkills blocked for %s", d)
	}
	waitFor(t, "run loop", f.running.Load)
	_ = a.stopSkills()
}

func TestStopSkillsGivesUpOnStuckLoop(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the 5s stop budget")
	}
	f := &fakeSyncer{ignoreCtx: true}
	a := newTestApp(t, &config.Config{Claude: claudeCfg()})
	a.skills = f
	a.startSkills()
	waitFor(t, "run loop", f.running.Load)
	start := time.Now()
	if err := a.stopSkills(); err == nil {
		t.Fatal("want a timeout error for a stuck loop")
	}
	if d := time.Since(start); d > skillsStopTimeout+2*time.Second {
		t.Fatalf("stopSkills took %s", d)
	}
}

func TestShutdownStopsSyncerAndClosesBackend(t *testing.T) {
	f := &fakeSyncer{}
	be := &closerBackend{}
	a := newTestApp(t, &config.Config{Claude: claudeCfg()})
	a.skills, a.claude = f, be
	a.startSkills()
	waitFor(t, "run loop", f.running.Load)
	if err := a.shutdown(); err != nil {
		t.Fatal(err)
	}
	if f.running.Load() {
		t.Error("syncer still running after shutdown")
	}
	if be.closed.Load() != 1 {
		t.Errorf("backend Close calls = %d, want 1", be.closed.Load())
	}
}

func TestShutdownWithoutCloserOrSkills(t *testing.T) {
	a := newTestApp(t, &config.Config{Claude: claudeCfg()})
	a.claude = &stubBackend{}
	if err := a.shutdown(); err != nil {
		t.Fatal(err)
	}
}

// Real syncer against a local file:// repository: initial sync populates the
// snapshot before startSkills returns, and shutdown leaves no goroutine behind.
func TestRealSyncerLifecycleAndRefresh(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	addSkill := func(name string) {
		dir := filepath.Join(repo, "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + name + "\ndescription: Skill " + name + ".\n---\nBody.\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-m", "add "+name)
	}
	git("init", "-b", "main")
	addSkill("alpha")

	stateDir := t.TempDir()
	// Snapshots are read-only; make them removable so TempDir cleanup works.
	t.Cleanup(func() {
		_ = filepath.WalkDir(stateDir, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	cfg := &config.Config{Claude: claudeCfg(), Skills: config.SkillsConfig{
		Repo: "file://" + repo, Ref: "main", Dir: stateDir, Refresh: 0, SyncTimeout: time.Minute,
	}}
	a := newTestApp(t, cfg)
	be := &closerBackend{}
	a.claude = be
	if err := a.initAndWire(); err != nil {
		t.Fatal(err)
	}
	before := runtime.NumGoroutine()
	a.startSkills()

	snap, ok := a.skills.Current()
	if !ok || len(snap.Skills) != 1 || snap.Skills[0].Name != "alpha" {
		t.Fatalf("snapshot after startSkills = %+v, %v", snap, ok)
	}
	if dir, ok := skillsProvider(a.skills)(); !ok || dir != snap.Dir {
		t.Fatalf("provider = %q, %v", dir, ok)
	}

	// Pick up a new commit through the internal refresh endpoint.
	addSkill("beta")
	internal := a.internalServer.(*muxServer)
	if rec := do(internal, "POST", "/api/v1/skills/refresh", "", ""); rec.Code != 202 {
		t.Fatalf("refresh = %d %s", rec.Code, rec.Body.String())
	}
	waitFor(t, "second skill", func() bool {
		s, _ := a.skills.Current()
		return len(s.Skills) == 2
	})
	rec := do(internal, "GET", "/api/v1/skills", "", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"beta"`) || !strings.Contains(rec.Body.String(), `"alpha"`) {
		t.Fatalf("GET skills = %d %s", rec.Code, rec.Body.String())
	}

	if err := a.shutdown(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "goroutines to drain", func() bool { return runtime.NumGoroutine() <= before })
}
