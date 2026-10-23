package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSkillsConfigValidate(t *testing.T) {
	valid := SkillsConfig{
		Repo:        "git@github.com:owner/repo.git",
		Ref:         "main",
		Dir:         "/var/lib/claudeai/skills",
		Refresh:     5 * time.Minute,
		SyncTimeout: 2 * time.Minute,
	}
	tests := []struct {
		name    string
		mutate  func(*SkillsConfig)
		wantErr string // empty means valid
	}{
		{"valid scp ssh url", func(c *SkillsConfig) {}, ""},
		{"disabled ignores everything else", func(c *SkillsConfig) { *c = SkillsConfig{Refresh: -1} }, ""},
		{"https url", func(c *SkillsConfig) { c.Repo = "https://github.com/owner/repo.git" }, ""},
		{"ssh url with git user", func(c *SkillsConfig) { c.Repo = "ssh://git@github.com/owner/repo.git" }, ""},
		{"file url", func(c *SkillsConfig) { c.Repo = "file:///srv/repo.git" }, ""},
		{"absolute local path", func(c *SkillsConfig) { c.Repo = "/srv/repo.git" }, ""},
		{"relative local path", func(c *SkillsConfig) { c.Repo = "../repo" }, ""},
		{"https credentials", func(c *SkillsConfig) { c.Repo = "https://user:s3cret@github.com/o/r.git" }, "embedded credentials"},
		{"https token as username", func(c *SkillsConfig) { c.Repo = "https://ghp_token@github.com/o/r.git" }, "embedded credentials"},
		{"ssh url with password", func(c *SkillsConfig) { c.Repo = "ssh://git:pw@github.com/o/r.git" }, "embedded credentials"},
		{"scp style with password", func(c *SkillsConfig) { c.Repo = "git:pw@github.com:o/r.git" }, "embedded credentials"},
		{"option injection", func(c *SkillsConfig) { c.Repo = "--upload-pack=evil" }, "cannot start with"},
		{"ext transport", func(c *SkillsConfig) { c.Repo = "ext::sh-evil" }, "unsupported"},
		{"unsupported scheme", func(c *SkillsConfig) { c.Repo = "ftp://host/repo" }, "unsupported"},
		{"whitespace in repo", func(c *SkillsConfig) { c.Repo = "https://host/a b" }, "whitespace"},
		{"empty ref", func(c *SkillsConfig) { c.Ref = " " }, "ref cannot be empty"},
		{"ref with option prefix", func(c *SkillsConfig) { c.Ref = "-x" }, "invalid skills ref"},
		{"ref with dotdot", func(c *SkillsConfig) { c.Ref = "a..b" }, "invalid skills ref"},
		{"ref with space", func(c *SkillsConfig) { c.Ref = "a b" }, "invalid skills ref"},
		{"full sha ref", func(c *SkillsConfig) { c.Ref = strings.Repeat("a", 40) }, ""},
		{"tag ref", func(c *SkillsConfig) { c.Ref = "v1.2.3" }, ""},
		{"empty dir", func(c *SkillsConfig) { c.Dir = "" }, "directory cannot be empty"},
		{"negative refresh", func(c *SkillsConfig) { c.Refresh = -time.Second }, "cannot be negative"},
		{"refresh zero", func(c *SkillsConfig) { c.Refresh = 0 }, ""},
		{"refresh below minimum", func(c *SkillsConfig) { c.Refresh = 10 * time.Second }, "at least 30s"},
		{"refresh at minimum", func(c *SkillsConfig) { c.Refresh = 30 * time.Second }, ""},
		{"zero sync timeout", func(c *SkillsConfig) { c.SyncTimeout = 0 }, "sync timeout must be positive"},
		{"key with newline", func(c *SkillsConfig) { c.SSHKey = "/k\nx" }, "control characters"},
		{"known hosts with quote", func(c *SkillsConfig) { c.KnownHosts = `/k"x` }, "quote"},
		{"valid key and known hosts", func(c *SkillsConfig) { c.SSHKey = "/k/key"; c.KnownHosts = "/k/known_hosts" }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid
			tt.mutate(&c)
			err := c.validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "s3cret") {
				t.Errorf("error leaks credentials: %v", err)
			}
		})
	}
}

func TestSkillsConfigEnabled(t *testing.T) {
	if (SkillsConfig{}).Enabled() {
		t.Error("empty repo must disable skills")
	}
	if !(SkillsConfig{Repo: "x"}).Enabled() {
		t.Error("repo set must enable skills")
	}
}

func TestLoadConfigSkills(t *testing.T) {
	prev := userCacheDir
	userCacheDir = func() (string, error) { return "/cache", nil }
	t.Cleanup(func() { userCacheDir = prev })

	t.Run("disabled by default", func(t *testing.T) {
		withCommandLine(t)
		cfg, cfgErr := LoadConfigFromEnv(fakeEnv(nil))
		if cfgErr != nil {
			t.Fatalf("LoadConfig failed: %v", cfgErr)
		}
		if cfg.Skills.Enabled() {
			t.Error("skills must be disabled without -skills-repo")
		}
		if cfg.Skills.Ref != "main" || cfg.Skills.Refresh != 5*time.Minute || cfg.Skills.SyncTimeout != 2*time.Minute {
			t.Errorf("unexpected defaults: %+v", cfg.Skills)
		}
	})

	t.Run("enabled with default dir", func(t *testing.T) {
		withCommandLine(t, "-skills-repo", "git@github.com:o/r.git")
		cfg, cfgErr := LoadConfigFromEnv(fakeEnv(nil))
		if cfgErr != nil {
			t.Fatalf("LoadConfig failed: %v", cfgErr)
		}
		want := filepath.Join("/cache", "claudeai-api", "skills")
		if cfg.Skills.Dir != want {
			t.Errorf("Dir = %q, want %q", cfg.Skills.Dir, want)
		}
	})

	t.Run("all flags", func(t *testing.T) {
		withCommandLine(t,
			"-skills-repo", "git@github.com:o/r.git",
			"-skills-ref", "v1",
			"-skills-dir", "/state",
			"-skills-refresh", "1m",
			"-skills-ssh-key", "/keys/id",
			"-skills-known-hosts", "/keys/kh",
			"-skills-sync-timeout", "30s",
		)
		cfg, cfgErr := LoadConfigFromEnv(fakeEnv(nil))
		if cfgErr != nil {
			t.Fatalf("LoadConfig failed: %v", cfgErr)
		}
		want := SkillsConfig{Repo: "git@github.com:o/r.git", Ref: "v1", Dir: "/state", Refresh: time.Minute, SSHKey: "/keys/id", KnownHosts: "/keys/kh", SyncTimeout: 30 * time.Second}
		if cfg.Skills != want {
			t.Errorf("Skills = %+v, want %+v", cfg.Skills, want)
		}
	})

	t.Run("credentials rejected through LoadConfig", func(t *testing.T) {
		withCommandLine(t, "-skills-repo", "https://u:p4ss@github.com/o/r.git")
		_, cfgErr := LoadConfigFromEnv(fakeEnv(nil))
		if cfgErr == nil || !strings.Contains(cfgErr.Error(), "embedded credentials") {
			t.Fatalf("expected credentials error, got %v", cfgErr)
		}
		if strings.Contains(cfgErr.Error(), "p4ss") {
			t.Errorf("error leaks the password: %v", cfgErr)
		}
	})

	t.Run("cache dir failure reported only when enabled", func(t *testing.T) {
		userCacheDir = func() (string, error) { return "", fmt.Errorf("no home") }
		defer func() { userCacheDir = func() (string, error) { return "/cache", nil } }()

		withCommandLine(t)
		if _, cfgErr := LoadConfigFromEnv(fakeEnv(nil)); cfgErr != nil {
			t.Fatalf("disabled skills must not need a cache dir: %v", cfgErr)
		}
		withCommandLine(t, "-skills-repo", "/srv/r.git")
		if _, cfgErr := LoadConfigFromEnv(fakeEnv(nil)); cfgErr == nil || !strings.Contains(cfgErr.Error(), "-skills-dir") {
			t.Fatalf("expected a hint to set -skills-dir, got %v", cfgErr)
		}
	})
}

func TestSkillsConfigFormatting(t *testing.T) {
	c := SkillsConfig{Repo: "git@github.com:o/r.git", Ref: "main", Dir: "/state", SSHKey: "/keys/id", KnownHosts: "/keys/kh", Refresh: time.Minute, SyncTimeout: time.Minute}
	for _, s := range []string{c.String(), c.GoString(), fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c)} {
		if !strings.Contains(s, "github.com:o/r.git") {
			t.Errorf("formatting should show the repo: %s", s)
		}
	}
	got := fmt.Sprint(c.LogValue())
	if !strings.Contains(got, "ssh_key_set=true") {
		t.Errorf("LogValue should report only whether a key is set, got %s", got)
	}
}
