package skills

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestFirstSyncMaterializesValidatedSkills(t *testing.T) {
	o := newOrigin(t)
	sha := o.commit("initial", marketplaceFiles(t, o))
	cfg := baseConfig(t, o.dir)
	s := newSyncer(t, cfg)

	if _, ok := s.Current(); ok {
		t.Fatal("Current must report no snapshot before the first sync")
	}
	snap := mustSync(t, s)

	if snap.Commit != sha || snap.Ref != "main" {
		t.Errorf("snapshot commit/ref = %s/%s, want %s/main", snap.Commit, snap.Ref, sha)
	}
	if got, want := skillNames(snap), []string{"summarize", "translate"}; !reflect.DeepEqual(got, want) {
		t.Errorf("skills = %v, want %v", got, want)
	}
	if snap.Skills[0].Plugin != "core" || snap.Skills[0].Description != "Summarize text." {
		t.Errorf("unexpected skill info: %+v", snap.Skills[0])
	}
	if snap.SyncedAt.IsZero() {
		t.Error("SyncedAt must be set")
	}
	if want := filepath.Join(cfg.Dir, "versions", sha); snap.Dir != want {
		t.Errorf("Dir = %s, want %s", snap.Dir, want)
	}

	data, err := os.ReadFile(filepath.Join(snap.Dir, ".claude", "skills", "summarize", "SKILL.md"))
	if err != nil || !strings.Contains(string(data), "name: summarize") {
		t.Fatalf("SKILL.md not materialized: %v %q", err, data)
	}
	if _, err := os.Stat(filepath.Join(snap.Dir, ".claude", "skills", "summarize", "reference.md")); err != nil {
		t.Errorf("supporting file missing: %v", err)
	}

	// Only validated skill content may exist: no hooks, MCP, agents, commands,
	// settings or invalid skills.
	for _, p := range walkFiles(t, snap.Dir) {
		if p == "." || p == ".claude" || p == ".claude/skills" {
			continue
		}
		if !strings.HasPrefix(p, ".claude/skills/summarize") && !strings.HasPrefix(p, ".claude/skills/translate") {
			t.Errorf("unexpected materialized path %q", p)
		}
	}

	wantSkipped := map[string]string{
		"skills/nofront":  "frontmatter",
		"skills/linky":    "symlink",
		"skills/big":      "too large",
		"skills/Bad_Name": "invalid skill directory name",
		"skills/mismatch": "does not match",
		"skills/nodesc":   "description",
	}
	for suffix, reason := range wantSkipped {
		got, ok := skippedReason(snap, suffix)
		if !ok {
			t.Errorf("skill %s should be skipped; skipped=%+v", suffix, snap.Skipped)
			continue
		}
		if !strings.Contains(got, reason) {
			t.Errorf("skip reason for %s = %q, want it to mention %q", suffix, got, reason)
		}
	}
	if r, ok := skippedReason(snap, "plugins/extra/skills/summarize"); !ok || !strings.Contains(r, "duplicate") {
		t.Errorf("duplicate skill should be skipped as duplicate, got %q %v", r, ok)
	}
	for _, name := range []string{"escape", "absolute", "remote"} {
		if _, ok := skippedReason(snap, "plugin:"+name); !ok {
			t.Errorf("plugin %s with a bad source should be reported as skipped; skipped=%+v", name, snap.Skipped)
		}
	}
	for _, sk := range snap.Skipped {
		if strings.Contains(sk.Path, "escaped") {
			t.Errorf("traversal source must not be followed: %+v", sk)
		}
	}
	if st := s.Status(); st.Commit != sha || st.Skills != 2 || st.ConsecutiveFailures != 0 || st.LastSuccess.IsZero() {
		t.Errorf("unexpected status: %+v", st)
	}
}

func TestReadOnlyPermissionsAndPrune(t *testing.T) {
	o := newOrigin(t)
	o.commit("c0", marketplaceFiles(t, o))
	cfg := baseConfig(t, o.dir)
	s := newSyncer(t, cfg)
	snap := mustSync(t, s)

	dirInfo, err := os.Stat(filepath.Join(snap.Dir, ".claude", "skills", "summarize"))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o555 {
		t.Errorf("skill dir mode = %v, want 0555", dirInfo.Mode().Perm())
	}
	fileInfo, err := os.Stat(filepath.Join(snap.Dir, ".claude", "skills", "summarize", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o444 {
		t.Errorf("file mode = %v, want 0444", fileInfo.Mode().Perm())
	}
	if err := os.WriteFile(filepath.Join(snap.Dir, ".claude", "skills", "summarize", "SKILL.md"), []byte("x"), 0o644); err == nil && os.Getuid() != 0 {
		t.Error("materialized files must not be writable")
	}

	// Five more commits: only the last three versions may remain.
	var shas []string
	shas = append(shas, snap.Commit)
	for i := 0; i < 5; i++ {
		shas = append(shas, o.commit("bump", map[string]string{
			"plugins/core/skills/translate/SKILL.md": skillMD("translate", "Translate text v"+string(rune('a'+i))),
		}))
		if err := s.Sync(context.Background()); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}
	got := versionDirs(t, cfg.Dir)
	if len(got) != 3 {
		t.Fatalf("versions kept = %v, want 3", got)
	}
	current, _ := s.Current()
	if current.Commit != shas[len(shas)-1] {
		t.Fatalf("current = %s, want %s", current.Commit, shas[len(shas)-1])
	}
	for _, want := range shas[len(shas)-3:] {
		if _, err := os.Stat(filepath.Join(cfg.Dir, "versions", want)); err != nil {
			t.Errorf("recent version %s should be kept: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.Dir, "versions", shas[0])); !os.IsNotExist(err) {
		t.Errorf("oldest version should be pruned, stat err = %v", err)
	}
}

func TestSecondSyncIsNoop(t *testing.T) {
	o := newOrigin(t)
	o.commit("c0", marketplaceFiles(t, o))
	cfg := baseConfig(t, o.dir)
	var infos []SyncInfo
	s := newSyncer(t, cfg, WithObserver(func(i SyncInfo) { infos = append(infos, i) }))

	first := mustSync(t, s)
	second := mustSync(t, s)
	if first.Commit != second.Commit || first.Dir != second.Dir {
		t.Errorf("snapshots differ: %+v vs %+v", first, second)
	}
	if n := len(versionDirs(t, cfg.Dir)); n != 1 {
		t.Errorf("version dirs = %d, want 1", n)
	}
	if len(infos) != 2 || infos[0].Outcome != OutcomeUpdated || infos[1].Outcome != OutcomeUnchanged {
		t.Errorf("observer outcomes = %+v", infos)
	}
}

func TestNewCommitSwapsAtomicallyAndKeepsOldVersionIntact(t *testing.T) {
	o := newOrigin(t)
	o.commit("c0", marketplaceFiles(t, o))
	cfg := baseConfig(t, o.dir)
	s := newSyncer(t, cfg)
	old := mustSync(t, s)

	held, err := os.Open(filepath.Join(old.Dir, ".claude", "skills", "translate", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	newSHA := o.commit("update translate", map[string]string{
		"plugins/core/skills/translate/SKILL.md": skillMD("translate", "Translate text, version two."),
	})

	// Concurrent readers must always see a complete, readable snapshot.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var bad atomic.Int32
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap, ok := s.Current()
				if !ok {
					bad.Add(1)
					return
				}
				if _, err := os.ReadFile(filepath.Join(snap.Dir, ".claude", "skills", "translate", "SKILL.md")); err != nil {
					bad.Add(1)
					return
				}
			}
		}()
	}
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
	if bad.Load() != 0 {
		t.Error("a reader observed a missing or unreadable snapshot during the swap")
	}

	cur, _ := s.Current()
	if cur.Commit != newSHA || cur.Dir == old.Dir {
		t.Fatalf("current did not move to the new version: %+v", cur)
	}
	oldData, err := io.ReadAll(held)
	if err != nil || !strings.Contains(string(oldData), "description: Translate text.") {
		t.Errorf("old version changed under a reader: %v %q", err, oldData)
	}
	again, err := os.ReadFile(filepath.Join(old.Dir, ".claude", "skills", "translate", "SKILL.md"))
	if err != nil || !strings.Contains(string(again), "Translate text.\n") {
		t.Errorf("old version dir not intact: %v %q", err, again)
	}
	newData, _ := os.ReadFile(filepath.Join(cur.Dir, ".claude", "skills", "translate", "SKILL.md"))
	if !strings.Contains(string(newData), "version two") {
		t.Errorf("new version has stale content: %q", newData)
	}
}

func TestRefPinning(t *testing.T) {
	o := newOrigin(t)
	c1 := o.commit("c1", marketplaceFiles(t, o))
	o.git("tag", "v1")
	o.git("checkout", "-q", "-b", "dev")
	d1 := o.commit("dev1", map[string]string{"plugins/core/skills/translate/SKILL.md": skillMD("translate", "dev one")})
	o.git("checkout", "-q", "main")
	c2 := o.commit("c2", map[string]string{"plugins/core/skills/translate/SKILL.md": skillMD("translate", "main two")})

	tests := []struct {
		name string
		ref  string
		want string
	}{
		{"branch main", "main", c2},
		{"tag", "v1", c1},
		{"full sha", c1, c1},
		{"other branch", "dev", d1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig(t, o.dir)
			cfg.Ref = tt.ref
			s := newSyncer(t, cfg)
			if got := mustSync(t, s).Commit; got != tt.want {
				t.Errorf("commit = %s, want %s", got, tt.want)
			}
		})
	}

	t.Run("pinned tag stays put while the branch moves", func(t *testing.T) {
		cfg := baseConfig(t, o.dir)
		cfg.Ref = "v1"
		s := newSyncer(t, cfg)
		mustSync(t, s)
		o.commit("c3", map[string]string{"plugins/core/skills/translate/SKILL.md": skillMD("translate", "main three")})
		if got := mustSync(t, s).Commit; got != c1 {
			t.Errorf("pinned commit moved to %s", got)
		}
	})

	t.Run("moving branch follows", func(t *testing.T) {
		cfg := baseConfig(t, o.dir)
		s := newSyncer(t, cfg)
		before := mustSync(t, s).Commit
		next := o.commit("c4", map[string]string{"plugins/core/skills/translate/SKILL.md": skillMD("translate", "main four")})
		if got := mustSync(t, s).Commit; got != next || got == before {
			t.Errorf("branch did not advance: before=%s after=%s want=%s", before, got, next)
		}
	})
}

func TestUnknownRefFailsAndKeepsLastGood(t *testing.T) {
	o := newOrigin(t)
	o.commit("c0", marketplaceFiles(t, o))

	t.Run("first sync with unknown ref", func(t *testing.T) {
		cfg := baseConfig(t, o.dir)
		cfg.Ref = "does-not-exist"
		s := newSyncer(t, cfg)
		err := s.Sync(context.Background())
		if !errors.Is(err, ErrRefNotFound) {
			t.Fatalf("err = %v, want ErrRefNotFound", err)
		}
		if _, ok := s.Current(); ok {
			t.Error("no snapshot expected")
		}
	})

	t.Run("branch deleted upstream keeps last good", func(t *testing.T) {
		o.git("checkout", "-q", "-b", "temp")
		o.commit("on temp", map[string]string{"temp.txt": "x"})
		cfg := baseConfig(t, o.dir)
		cfg.Ref = "temp"
		s := newSyncer(t, cfg)
		good := mustSync(t, s)
		o.git("checkout", "-q", "main")
		o.git("branch", "-D", "temp")

		err := s.Sync(context.Background())
		if !errors.Is(err, ErrRefNotFound) {
			t.Fatalf("err = %v, want ErrRefNotFound", err)
		}
		cur, ok := s.Current()
		if !ok || cur.Commit != good.Commit {
			t.Errorf("last good snapshot lost: %+v %v", cur, ok)
		}
		if st := s.Status(); st.ConsecutiveFailures != 1 || st.LastError == "" {
			t.Errorf("status = %+v", st)
		}
	})
}

func TestUnreachableRemoteKeepsLastGoodAndRecovers(t *testing.T) {
	o := newOrigin(t)
	o.commit("c0", marketplaceFiles(t, o))
	cfg := baseConfig(t, o.dir)
	var last SyncInfo
	s := newSyncer(t, cfg, WithObserver(func(i SyncInfo) { last = i }))
	good := mustSync(t, s)

	backup := o.dir + ".bak"
	if err := os.Rename(o.dir, backup); err != nil {
		t.Fatal(err)
	}
	for want := 1; want <= 2; want++ {
		if err := s.Sync(context.Background()); err == nil {
			t.Fatal("sync against a missing remote must fail")
		}
		if st := s.Status(); st.ConsecutiveFailures != want || st.LastAttempt.IsZero() || st.LastError == "" {
			t.Errorf("status after %d failures = %+v", want, st)
		}
	}
	if last.Outcome != OutcomeError || last.Err == nil {
		t.Errorf("observer should see the error: %+v", last)
	}
	if cur, ok := s.Current(); !ok || cur.Commit != good.Commit {
		t.Errorf("last good snapshot not retained: %+v", cur)
	}

	if err := os.Rename(backup, o.dir); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync after recovery: %v", err)
	}
	if st := s.Status(); st.ConsecutiveFailures != 0 || st.LastError != "" {
		t.Errorf("status not reset after recovery: %+v", st)
	}
}

func TestRootSkillsFallback(t *testing.T) {
	o := newOrigin(t)
	o.commit("c0", map[string]string{
		"skills/hello/SKILL.md": skillMD("hello", "Say hello."),
		"skills/stray.txt":      "ignored",
	})
	s := newSyncer(t, baseConfig(t, o.dir))
	snap := mustSync(t, s)
	if got := skillNames(snap); !reflect.DeepEqual(got, []string{"hello"}) {
		t.Errorf("skills = %v", got)
	}
	if snap.Skills[0].Plugin != "" {
		t.Errorf("root skills have no plugin, got %q", snap.Skills[0].Plugin)
	}
}

func TestMalformedMarketplaceFallsBackAndReports(t *testing.T) {
	o := newOrigin(t)
	o.commit("c0", map[string]string{
		".claude-plugin/marketplace.json": "{not json",
		"skills/hello/SKILL.md":           skillMD("hello", "Say hello."),
	})
	snap := mustSync(t, newSyncer(t, baseConfig(t, o.dir)))
	if got := skillNames(snap); !reflect.DeepEqual(got, []string{"hello"}) {
		t.Errorf("skills = %v", got)
	}
	if _, ok := skippedReason(snap, "marketplace.json"); !ok {
		t.Errorf("malformed marketplace should be reported: %+v", snap.Skipped)
	}
}

func TestSkillLimits(t *testing.T) {
	o := newOrigin(t)
	files := map[string]string{}
	for i := 0; i < maxSkills+3; i++ {
		name := "s" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		files["skills/"+name+"/SKILL.md"] = skillMD(name, "d")
	}
	many := map[string]string{"skills/many/SKILL.md": skillMD("many", "d")}
	for i := 0; i <= maxFilesPerSkill; i++ {
		many["skills/many/f"+string(rune('a'+i/26))+string(rune('a'+i%26))+".txt"] = "x"
	}
	chunk := strings.Repeat("y", maxFileBytes-10)
	heavy := map[string]string{"skills/heavy/SKILL.md": skillMD("heavy", "d")}
	for i := 0; i < 9; i++ {
		heavy["skills/heavy/part"+string(rune('a'+i))+".txt"] = chunk
	}
	for _, m := range []map[string]string{many, heavy} {
		for k, v := range m {
			files[k] = v
		}
	}
	o.commit("limits", files)
	snap := mustSync(t, newSyncer(t, baseConfig(t, o.dir)))

	if len(snap.Skills) != maxSkills {
		t.Errorf("skills = %d, want cap %d", len(snap.Skills), maxSkills)
	}
	reasons := map[string]string{}
	capped := 0
	for _, sk := range snap.Skipped {
		reasons[sk.Path] = sk.Reason
		if strings.Contains(sk.Reason, "too many skills") {
			capped++
		}
	}
	if !strings.Contains(reasons["skills/many"], "files") {
		t.Errorf("many reason = %q", reasons["skills/many"])
	}
	if !strings.Contains(reasons["skills/heavy"], "size") {
		t.Errorf("heavy reason = %q", reasons["skills/heavy"])
	}
	if capped != 3 {
		t.Errorf("skills beyond the cap reported = %d, want 3", capped)
	}
}

func TestSingleFlight(t *testing.T) {
	o := newOrigin(t)
	o.commit("c0", marketplaceFiles(t, o))
	realGit := requireGit(t)
	bin, calls := fakeGit(t, "case \"$*\" in *clone*) sleep 0.4;; esac\nexec '"+realGit+"' \"$@\"\n")
	s := newSyncer(t, baseConfig(t, o.dir), WithGit(bin))

	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.Sync(context.Background()) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("sync: %v", err)
		}
	}
	if n := countLines(t, calls, "clone"); n != 1 {
		t.Errorf("git clone ran %d times, want 1 (single flight)", n)
	}
}

func TestSyncTimeoutKillsGit(t *testing.T) {
	o := newOrigin(t)
	o.commit("c0", marketplaceFiles(t, o))
	bin, _ := fakeGit(t, "sleep 30\n")
	cfg := baseConfig(t, o.dir)
	cfg.SyncTimeout = 300 * time.Millisecond
	s := newSyncer(t, cfg, WithGit(bin))

	start := time.Now()
	err := s.Sync(context.Background())
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("sync took %s; the hung git process was not killed", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error should mention the timeout: %v", err)
	}
}

func TestGitEnvironmentAndErrorSanitizing(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "very-secret-name_key")
	khPath := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(keyPath, []byte("-----BEGIN PRIVATE KEY-----\nTOPSECRETKEYMATERIAL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(khPath, []byte("github.com ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(t.TempDir(), "env.txt")
	bin, _ := fakeGit(t, "env > '"+envFile+"'\necho \"Load key \\\"'"+keyPath+"'\\\": bad TOPSECRETKEYMATERIAL\" >&2\nexit 128\n")

	cfg := baseConfig(t, "git@github.com:owner/repo.git")
	cfg.SSHKey, cfg.KnownHosts = keyPath, khPath
	s := newSyncer(t, cfg, WithGit(bin))

	err := s.Sync(context.Background())
	if err == nil {
		t.Fatal("expected failure")
	}
	if strings.Contains(err.Error(), keyPath) {
		t.Errorf("error leaks the key path: %v", err)
	}
	st := s.Status()
	if strings.Contains(st.LastError, keyPath) {
		t.Errorf("status leaks the key path: %v", st.LastError)
	}

	data, rerr := os.ReadFile(envFile)
	if rerr != nil {
		t.Fatal(rerr)
	}
	env := string(data)
	for _, want := range []string{"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -i '" + keyPath + "'", "IdentitiesOnly=yes", "IdentityAgent=none", "BatchMode=yes", "StrictHostKeyChecking=yes", "UserKnownHostsFile=" + khPath} {
		if !strings.Contains(env, want) {
			t.Errorf("git env missing %q", want)
		}
	}
	if strings.Contains(env, "StrictHostKeyChecking=no") {
		t.Error("host key checking must never be disabled")
	}
	if strings.Contains(env, "TOPSECRETKEYMATERIAL") {
		t.Error("key material must never reach the environment")
	}
}

func TestRunPicksUpNewCommitsAndStops(t *testing.T) {
	o := newOrigin(t)
	o.commit("c0", marketplaceFiles(t, o))
	cfg := baseConfig(t, o.dir)
	s := newSyncer(t, cfg, WithInterval(40*time.Millisecond), WithLogger(slog.New(slog.DiscardHandler)))

	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	waitFor(t, 15*time.Second, func() bool { _, ok := s.Current(); return ok })
	first, _ := s.Current()
	next := o.commit("c1", map[string]string{"plugins/core/skills/translate/SKILL.md": skillMD("translate", "run loop update")})
	waitFor(t, 15*time.Second, func() bool { c, _ := s.Current(); return c.Commit == next })
	if c, _ := s.Current(); c.Commit == first.Commit {
		t.Error("Run did not pick up the new commit")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop after the context was canceled")
	}
	waitFor(t, 5*time.Second, func() bool { return runtime.NumGoroutine() <= before+1 })
}

func TestRunRefreshZeroSyncsOnceButHonoursTrigger(t *testing.T) {
	o := newOrigin(t)
	o.commit("c0", marketplaceFiles(t, o))
	bin, calls := fakeGit(t, "")
	s := newSyncer(t, baseConfig(t, o.dir), WithGit(bin), WithInterval(0))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	waitFor(t, 15*time.Second, func() bool { _, ok := s.Current(); return ok })
	time.Sleep(150 * time.Millisecond)
	if n := countLines(t, calls, "fetch"); n != 0 {
		t.Errorf("refresh=0 must not re-sync periodically, fetch ran %d times", n)
	}
	next := o.commit("c1", map[string]string{"plugins/core/skills/translate/SKILL.md": skillMD("translate", "triggered")})
	s.Trigger()
	waitFor(t, 15*time.Second, func() bool { c, _ := s.Current(); return c.Commit == next })
	cancel()
	<-done
}

func TestTriggerCoalesces(t *testing.T) {
	o := newOrigin(t)
	o.commit("c0", marketplaceFiles(t, o))
	s := newSyncer(t, baseConfig(t, o.dir))
	for i := 0; i < 50; i++ {
		s.Trigger() // must never block
	}
	if got := len(s.trigger); got != 1 {
		t.Errorf("pending triggers = %d, want 1", got)
	}
}

func TestSpanAndObserver(t *testing.T) {
	o := newOrigin(t)
	sha := o.commit("c0", marketplaceFiles(t, o))
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	var info SyncInfo
	s := newSyncer(t, baseConfig(t, o.dir), WithTracerProvider(tp), WithObserver(func(i SyncInfo) { info = i }))
	mustSync(t, s)

	spans := rec.Ended()
	if len(spans) != 1 || spans[0].Name() != "skills.sync" {
		t.Fatalf("spans = %v", spans)
	}
	attrs := map[string]attribute.Value{}
	for _, kv := range spans[0].Attributes() {
		attrs[string(kv.Key)] = kv.Value
	}
	if attrs["skills.commit"].AsString() != sha || attrs["skills.outcome"].AsString() != OutcomeUpdated ||
		attrs["skills.count"].AsInt64() != 2 || attrs["skills.skipped"].AsInt64() < 6 || attrs["skills.ref"].AsString() != "main" {
		t.Errorf("span attributes = %v", attrs)
	}
	if strings.Contains(attrs["skills.repo"].AsString(), "@") {
		t.Errorf("repo attribute must not carry credentials: %v", attrs["skills.repo"])
	}
	if info.Outcome != OutcomeUpdated || info.Commit != sha || info.Skills != 2 || info.Duration <= 0 {
		t.Errorf("observer info = %+v", info)
	}

	// A failing sync records the error on the span.
	rec2 := tracetest.NewSpanRecorder()
	cfg := baseConfig(t, filepath.Join(t.TempDir(), "missing"))
	s2 := newSyncer(t, cfg, WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec2))))
	if err := s2.Sync(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	failed := rec2.Ended()
	if len(failed) != 1 || len(failed[0].Events()) == 0 || failed[0].Status().Code == 0 {
		t.Errorf("failed span should carry the error: %+v", failed)
	}
}

func TestNewValidation(t *testing.T) {
	requireGit(t)
	t.Run("disabled", func(t *testing.T) {
		if _, err := New(baseConfig(t, "")); !errors.Is(err, ErrDisabled) {
			t.Errorf("err = %v, want ErrDisabled", err)
		}
	})
	t.Run("credentials in url", func(t *testing.T) {
		_, err := New(baseConfig(t, "https://user:hunter2@example.com/o/r.git"))
		if err == nil || !strings.Contains(err.Error(), "credentials") || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("git missing", func(t *testing.T) {
		_, err := New(baseConfig(t, "/tmp/repo"), WithGit("/definitely/not/git"))
		if !errors.Is(err, ErrGitNotFound) {
			t.Errorf("err = %v, want ErrGitNotFound", err)
		}
	})
	t.Run("missing key file", func(t *testing.T) {
		cfg := baseConfig(t, "git@github.com:o/r.git")
		cfg.SSHKey = filepath.Join(t.TempDir(), "nope")
		if _, err := New(cfg); err == nil {
			t.Error("a configured but missing key file must be rejected at startup")
		}
	})
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
