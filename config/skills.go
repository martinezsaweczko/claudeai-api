package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// userCacheDir is os.UserCacheDir, replaceable in tests.
var userCacheDir = os.UserCacheDir

// minSkillsRefresh is the shortest allowed periodic refresh; faster polling
// would hammer the git remote for no benefit.
const minSkillsRefresh = 30 * time.Second

var (
	// scpLikeRe matches git's scp-like syntax: user@host:path.
	scpLikeRe = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:.+$`)
	// skillsRefRe is the allowed charset of a branch, tag or commit sha.
	skillsRefRe = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)
)

// SkillsConfig configures the optional skills repository the gateway syncs and
// exposes to the Claude CLI. Skills are disabled when Repo is empty. Only paths
// are configured: no secret value is ever held here (the SSH key is a path to
// a file).
type SkillsConfig struct {
	Repo        string        // git URL (ssh, https, file://) or local path; empty disables skills
	Ref         string        // branch, tag or full commit sha
	Dir         string        // state directory (clone cache and materialized versions)
	Refresh     time.Duration // periodic re-sync interval; 0 syncs once at startup
	SSHKey      string        // path to a private key file (optional)
	KnownHosts  string        // path to a known_hosts file (optional)
	SyncTimeout time.Duration // max wall time of one sync (all git operations)
}

// Enabled reports whether a skills repository is configured.
func (c SkillsConfig) Enabled() bool { return strings.TrimSpace(c.Repo) != "" }

// Validate checks the configuration; a disabled (empty repo) config is valid.
func (c SkillsConfig) Validate() error { return c.validate() }

func (c SkillsConfig) validate() error {
	if !c.Enabled() {
		return nil
	}
	var problems []string
	if err := validateSkillsRepo(c.Repo); err != nil {
		problems = append(problems, err.Error())
	}
	switch ref := strings.TrimSpace(c.Ref); {
	case ref == "":
		problems = append(problems, "skills ref cannot be empty")
	case !skillsRefRe.MatchString(ref) || strings.HasPrefix(ref, "-") || strings.Contains(ref, ".."):
		problems = append(problems, fmt.Sprintf("invalid skills ref %q: use a branch, tag or full commit sha", ref))
	}
	if strings.TrimSpace(c.Dir) == "" {
		problems = append(problems, "skills directory cannot be empty")
	}
	switch {
	case c.Refresh < 0:
		problems = append(problems, "skills refresh cannot be negative")
	case c.Refresh > 0 && c.Refresh < minSkillsRefresh:
		problems = append(problems, fmt.Sprintf("skills refresh must be 0 or at least %s", minSkillsRefresh))
	}
	if c.SyncTimeout <= 0 {
		problems = append(problems, "skills sync timeout must be positive")
	}
	for name, p := range map[string]string{"ssh key": c.SSHKey, "known hosts": c.KnownHosts} {
		if err := validateSkillsPath(name, p); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// validateSkillsPath rejects characters that cannot be passed safely through an
// ssh command line.
func validateSkillsPath(what, p string) error {
	if strings.ContainsFunc(p, unicode.IsControl) {
		return fmt.Errorf("skills %s path cannot contain control characters", what)
	}
	if strings.ContainsAny(p, `"`) {
		return fmt.Errorf("skills %s path cannot contain a double quote", what)
	}
	return nil
}

// validateSkillsRepo accepts ssh (scp-like or ssh://), https/http, file:// and
// local paths, and rejects option-like values, exotic git transports and any
// URL that embeds credentials. The error never echoes the credentials.
func validateSkillsRepo(raw string) error {
	repo := strings.TrimSpace(raw)
	if strings.ContainsFunc(repo, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return fmt.Errorf("skills repo cannot contain whitespace or control characters")
	}
	if strings.HasPrefix(repo, "-") {
		return fmt.Errorf("skills repo cannot start with '-'")
	}
	if strings.Contains(repo, "://") {
		u, err := url.Parse(repo)
		if err != nil {
			return fmt.Errorf("skills repo is not a valid URL")
		}
		switch u.Scheme {
		case "ssh":
			if _, hasPassword := u.User.Password(); hasPassword {
				return fmt.Errorf("skills repo URL must not contain embedded credentials; use -skills-ssh-key")
			}
		case "https", "http", "file":
			if u.User != nil {
				return fmt.Errorf("skills repo URL must not contain embedded credentials; use an ssh URL with -skills-ssh-key")
			}
		default:
			return fmt.Errorf("unsupported skills repo scheme %q (use ssh, https or file)", u.Scheme)
		}
		return nil
	}
	if strings.Contains(repo, "::") {
		return fmt.Errorf("unsupported skills repo transport")
	}
	if strings.HasPrefix(repo, "/") || strings.HasPrefix(repo, "./") || strings.HasPrefix(repo, "../") {
		return nil
	}
	if at := strings.Index(repo, "@"); at >= 0 {
		if strings.Contains(repo[:at], ":") {
			return fmt.Errorf("skills repo URL must not contain embedded credentials; use -skills-ssh-key")
		}
		if !scpLikeRe.MatchString(repo) {
			return fmt.Errorf("unsupported skills repo %q", "<redacted>")
		}
	}
	return nil
}

// defaultSkillsDir returns <user cache dir>/claudeai-api/skills.
func defaultSkillsDir() (string, error) {
	base, err := userCacheDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine the default skills directory, set -skills-dir: %v", err)
	}
	return filepath.Join(base, "claudeai-api", "skills"), nil
}

// String shows paths and settings; there is no secret value to hide (the SSH
// key is only a path).
func (c SkillsConfig) String() string {
	return fmt.Sprintf("{Repo:%s Ref:%s Dir:%s Refresh:%s SyncTimeout:%s SSHKey:%s KnownHosts:%s}",
		c.Repo, c.Ref, c.Dir, c.Refresh, c.SyncTimeout, c.SSHKey, c.KnownHosts)
}

// GoString keeps %#v consistent with String.
func (c SkillsConfig) GoString() string { return "config.SkillsConfig" + c.String() }

// LogValue implements slog.LogValuer. The key path is reduced to a flag.
func (c SkillsConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Bool("enabled", c.Enabled()),
		slog.String("repo", c.Repo),
		slog.String("ref", c.Ref),
		slog.String("dir", c.Dir),
		slog.Duration("refresh", c.Refresh),
		slog.Duration("sync_timeout", c.SyncTimeout),
		slog.Bool("ssh_key_set", c.SSHKey != ""),
		slog.Bool("known_hosts_set", c.KnownHosts != ""),
	)
}
