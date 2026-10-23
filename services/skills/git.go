package skills

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"unicode"
)

const (
	maxErrBytes    = 2 << 10  // cap on an error message built from git stderr
	maxStderrBytes = 8 << 10  // cap on captured stderr
	maxStdoutBytes = 64 << 20 // cap on captured stdout (tree listings)
)

var scpRepoRe = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:.+$`)

// isSSHRepo reports whether git will reach repo over ssh.
func isSSHRepo(repo string) bool {
	if strings.HasPrefix(repo, "ssh://") {
		return true
	}
	if strings.Contains(repo, "://") || strings.HasPrefix(repo, "/") || strings.HasPrefix(repo, ".") {
		return false
	}
	return scpRepoRe.MatchString(repo)
}

// shellQuote single-quotes s for a POSIX shell (git runs GIT_SSH_COMMAND
// through the shell).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sshOptValue double-quotes a value for ssh's own option parser when it
// contains whitespace.
func sshOptValue(v string) string {
	if strings.ContainsAny(v, " \t") {
		return `"` + v + `"`
	}
	return v
}

// buildSSHCommand returns the GIT_SSH_COMMAND value. It never disables host key
// verification: with no known_hosts file configured ssh falls back to the
// system one, and BatchMode makes an unknown host fail instead of prompting.
func buildSSHCommand(keyPath, knownHostsPath string) string {
	parts := []string{"ssh"}
	if keyPath != "" {
		parts = append(parts, "-i", shellQuote(keyPath), "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none")
	}
	parts = append(parts, "-o", "BatchMode=yes")
	if knownHostsPath != "" {
		parts = append(parts, "-o", shellQuote("UserKnownHostsFile="+sshOptValue(knownHostsPath)))
	}
	parts = append(parts, "-o", "StrictHostKeyChecking=yes")
	return strings.Join(parts, " ")
}

// sanitize strips control characters, redacts each non-empty secret and caps
// the length of s so it is safe to log or return.
func sanitize(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "<redacted>")
		}
	}
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxErrBytes {
		s = s[:maxErrBytes] + "..."
	}
	return s
}

// gitRunner executes the git binary with a locked-down environment.
type gitRunner struct {
	bin      string
	env      []string
	gitDir   string // set after the cache clone exists
	workDir  string // process cwd
	redacted []string
}

// gitEnv builds the child environment: inherited variables except GIT_* ones,
// plus settings that make git strictly non-interactive and ignore any ambient
// git configuration or exotic transport.
func gitEnvironment(base []string, ssh string) []string {
	out := make([]string, 0, len(base)+12)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") || k == "SSH_ASKPASS" || k == "SSH_ASKPASS_REQUIRE" || k == "GCM_INTERACTIVE" {
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"SSH_ASKPASS_REQUIRE=never",
		"GCM_INTERACTIVE=never",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_ALLOW_PROTOCOL=file:ssh:https:http",
		"GIT_OPTIONAL_LOCKS=0",
		"LC_ALL=C",
	)
	if ssh != "" {
		out = append(out, "GIT_SSH_COMMAND="+ssh)
	}
	return out
}

// run executes git with args and returns stdout. stdout is capped at limit
// bytes (exceeding it is an error). A timeout or cancel kills the whole
// process group.
func (g *gitRunner) run(ctx context.Context, stdin []byte, limit int, args ...string) ([]byte, error) {
	full := []string{"-c", "credential.helper=", "-c", "protocol.ext.allow=never", "-c", "core.hooksPath=" + os.DevNull}
	if g.gitDir != "" {
		full = append(full, "--git-dir="+g.gitDir)
	}
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, g.bin, full...)
	cmd.Dir = g.workDir
	cmd.Env = g.env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	stdout := &limitedBuffer{max: limit}
	stderr := &limitedBuffer{max: maxStderrBytes}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	configureProcess(cmd)

	err := cmd.Run()
	op := args[0]
	if i := indexOfSubcommand(args); i >= 0 {
		op = args[i]
	}
	switch {
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, fmt.Errorf("git %s timed out", op)
	case ctx.Err() != nil:
		return nil, fmt.Errorf("git %s canceled: %w", op, ctx.Err())
	case stdout.exceeded:
		return nil, fmt.Errorf("git %s: output exceeds %d bytes", op, limit)
	case err != nil:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, &gitError{op: op, msg: sanitize(stderr.String(), g.redacted...), code: ee.ExitCode()}
		}
		return nil, fmt.Errorf("git %s: %s", op, sanitize(err.Error(), g.redacted...))
	}
	return stdout.Bytes(), nil
}

func indexOfSubcommand(args []string) int {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			return i
		}
	}
	return -1
}

// gitError is a non-zero git exit with its sanitized stderr.
type gitError struct {
	op   string
	msg  string
	code int
}

func (e *gitError) Error() string {
	if e.msg == "" {
		return fmt.Sprintf("git %s failed (exit %d)", e.op, e.code)
	}
	return fmt.Sprintf("git %s failed (exit %d): %s", e.op, e.code, e.msg)
}

// limitedBuffer keeps at most max bytes. For stdout, overflow is flagged; for
// stderr it is silently truncated (exceeded is ignored there).
type limitedBuffer struct {
	buf      bytes.Buffer
	max      int
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room < len(p) {
		b.exceeded = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *limitedBuffer) String() string { return b.buf.String() }
