package skills

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/martinezsaweczko/claudeai-api/config"
)

func requireGit(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	return bin
}

func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fakes need a unix shell")
	}
}

// origin is a throwaway non-bare git repository acting as the remote.
type origin struct {
	t    *testing.T
	dir  string
	seq  int
	mu   sync.Mutex
	base []string
}

func gitEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
	)
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func newOrigin(t *testing.T) *origin {
	t.Helper()
	requireGit(t)
	o := &origin{t: t, dir: filepath.Join(t.TempDir(), "origin")}
	if err := os.MkdirAll(o.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, o.dir, "init", "-q", "-b", "main")
	return o
}

// commit writes files (an empty content removes the file), commits and returns the sha.
func (o *origin) commit(msg string, files map[string]string) string {
	o.t.Helper()
	for p, content := range files {
		full := filepath.Join(o.dir, filepath.FromSlash(p))
		if content == "" {
			_ = os.Remove(full)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			o.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			o.t.Fatal(err)
		}
	}
	return o.commitAll(msg)
}

func (o *origin) symlink(path, target string) {
	o.t.Helper()
	full := filepath.Join(o.dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		o.t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		o.t.Skipf("symlinks unavailable: %v", err)
	}
}

func (o *origin) commitAll(msg string) string {
	o.t.Helper()
	runGit(o.t, o.dir, "add", "-A")
	runGit(o.t, o.dir, "commit", "-q", "--allow-empty", "-m", msg)
	return runGit(o.t, o.dir, "rev-parse", "HEAD")
}

func (o *origin) git(args ...string) string { o.t.Helper(); return runGit(o.t, o.dir, args...) }

func skillMD(name, desc string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n# " + name + "\n"
}

const marketplaceJSON = `{
  "name": "api-skills",
  "plugins": [
    {"name": "core", "source": "./plugins/core"},
    {"name": "escape", "source": "../outside"},
    {"name": "absolute", "source": "/etc"},
    {"name": "remote", "source": {"source": "github", "repo": "x/y"}},
    {"name": "extra", "source": "./plugins/extra"}
  ]
}`

// marketplaceFiles is the baseline fixture: two valid skills plus a collection
// of invalid ones and executable-looking plugin content that must never be
// materialized.
func marketplaceFiles(t *testing.T, o *origin) map[string]string {
	big := strings.Repeat("x", maxFileBytes+1)
	files := map[string]string{
		".claude-plugin/marketplace.json":            marketplaceJSON,
		"plugins/core/skills/summarize/SKILL.md":     skillMD("summarize", "Summarize text."),
		"plugins/core/skills/summarize/reference.md": "extra reference\n",
		"plugins/core/skills/translate/SKILL.md":     skillMD("translate", "Translate text."),
		"plugins/core/skills/nofront/SKILL.md":       "# no frontmatter\n",
		"plugins/core/skills/linky/SKILL.md":         skillMD("linky", "Has a symlink."),
		"plugins/core/skills/big/SKILL.md":           skillMD("big", "Has an oversized file."),
		"plugins/core/skills/big/blob.txt":           big,
		"plugins/core/skills/Bad_Name/SKILL.md":      skillMD("Bad_Name", "Bad dir name."),
		"plugins/core/skills/mismatch/SKILL.md":      skillMD("other", "Name differs from dir."),
		"plugins/core/skills/nodesc/SKILL.md":        "---\nname: nodesc\n---\nbody\n",
		"plugins/extra/skills/summarize/SKILL.md":    skillMD("summarize", "Duplicate of core."),
		"plugins/core/hooks/hooks.json":              `{"hooks":{}}`,
		"plugins/core/.mcp.json":                     `{"mcpServers":{}}`,
		"plugins/core/agents/evil.md":                "agent\n",
		"plugins/core/commands/evil.md":              "command\n",
		"plugins/core/.claude-plugin/plugin.json":    `{"name":"core"}`,
		".claude/settings.json":                      `{"hooks":{}}`,
		"outside/skills/escaped/SKILL.md":            skillMD("escaped", "Reachable only via traversal."),
		"README.md":                                  "readme\n",
	}
	o.symlink("plugins/core/skills/linky/link", "/etc/passwd")
	return files
}

func baseConfig(t *testing.T, repo string) config.SkillsConfig {
	t.Helper()
	return config.SkillsConfig{
		Repo:        repo,
		Ref:         "main",
		Dir:         filepath.Join(t.TempDir(), "state"),
		Refresh:     5 * time.Minute,
		SyncTimeout: 30 * time.Second,
	}
}

func newSyncer(t *testing.T, cfg config.SkillsConfig, opts ...Option) *Syncer {
	t.Helper()
	requireGit(t)
	s, err := New(cfg, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = removeAll(cfg.Dir) })
	return s
}

func mustSync(t *testing.T, s *Syncer) Snapshot {
	t.Helper()
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	snap, ok := s.Current()
	if !ok {
		t.Fatal("no current snapshot after a successful sync")
	}
	return snap
}

func skillNames(snap Snapshot) []string {
	names := make([]string, 0, len(snap.Skills))
	for _, s := range snap.Skills {
		names = append(names, s.Name)
	}
	return names
}

func versionDirs(t *testing.T, stateDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(stateDir, "versions"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func skippedReason(snap Snapshot, pathSuffix string) (string, bool) {
	for _, s := range snap.Skipped {
		if strings.HasSuffix(s.Path, pathSuffix) {
			return s.Reason, true
		}
	}
	return "", false
}

func walkFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// fakeGit writes an executable script that logs its arguments to logFile and
// then runs body ("" means exec the real git).
func fakeGit(t *testing.T, body string) (bin, logFile string) {
	t.Helper()
	requireUnix(t)
	real := requireGit(t)
	dir := t.TempDir()
	logFile = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$@\" >> '" + logFile + "'\n"
	if body == "" {
		body = "exec '" + real + "' \"$@\"\n"
	}
	bin = filepath.Join(dir, "git")
	if err := os.WriteFile(bin, []byte(script+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logFile
}

func countLines(t *testing.T, file, contains string) int {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, l := range strings.Split(string(data), "\n") {
		if l != "" && strings.Contains(l, contains) {
			n++
		}
	}
	return n
}
