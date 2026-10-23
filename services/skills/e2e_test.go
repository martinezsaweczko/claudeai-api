package skills

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/martinezsaweczko/claudeai-api/config"
)

// TestE2ERealRepo syncs a real (possibly private) repository. It is skipped
// unless CLAUDEAI_SKILLS_E2E=1 and reads only paths from the environment:
// CLAUDEAI_SKILLS_E2E_REPO, _KEY, _KNOWN_HOSTS and optionally _DIR (state dir
// that is kept, so a caller can reuse the materialized snapshot).
func TestE2ERealRepo(t *testing.T) {
	if os.Getenv("CLAUDEAI_SKILLS_E2E") != "1" {
		t.Skip("set CLAUDEAI_SKILLS_E2E=1 to run against a real repository")
	}
	requireGit(t)
	dir := os.Getenv("CLAUDEAI_SKILLS_E2E_DIR")
	if dir == "" {
		dir = t.TempDir() + "/state"
		t.Cleanup(func() { _ = removeAll(dir) })
	}
	s, err := New(config.SkillsConfig{
		Repo:        os.Getenv("CLAUDEAI_SKILLS_E2E_REPO"),
		Ref:         "main",
		Dir:         dir,
		SSHKey:      os.Getenv("CLAUDEAI_SKILLS_E2E_KEY"),
		KnownHosts:  os.Getenv("CLAUDEAI_SKILLS_E2E_KNOWN_HOSTS"),
		Refresh:     5 * time.Minute,
		SyncTimeout: 2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Current()
	t.Logf("commit=%s ref=%s dir=%s", snap.Commit, snap.Ref, snap.Dir)
	for _, sk := range snap.Skills {
		t.Logf("skill %s (plugin %s): %s", sk.Name, sk.Plugin, sk.Description)
	}
	for _, sk := range snap.Skipped {
		t.Logf("skipped %s: %s", sk.Path, sk.Reason)
	}
	if len(snap.Skills) == 0 {
		t.Error("expected at least one skill")
	}
}
