package config

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Environment variables holding secrets. Secrets are read from the
// environment, not flags, because flags are visible in process listings.
const (
	envOAuthToken = "CLAUDE_CODE_OAUTH_TOKEN"
	envAPIKeys    = "CLAUDEAI_API_KEYS"
)

// ClaudeConfig configures the Claude Code CLI backend and the optional
// client API-key authentication.
type ClaudeConfig struct {
	Bin            string        // Claude Code CLI binary name or path
	Model          string        // Default model alias when a request names none
	Timeout        time.Duration // Max wall time per request
	MaxConcurrency int           // Max simultaneous CLI processes
	OAuthToken     string        `json:"-"` // CLAUDE_CODE_OAUTH_TOKEN (secret, optional)
	APIKeys        []string      `json:"-"` // CLAUDEAI_API_KEYS (secrets, optional); empty disables auth
}

func (c ClaudeConfig) validate() error {
	var problems []string
	if strings.TrimSpace(c.Bin) == "" {
		problems = append(problems, "claude binary cannot be empty")
	}
	if strings.TrimSpace(c.Model) == "" {
		problems = append(problems, "claude model cannot be empty")
	}
	if c.Timeout <= 0 {
		problems = append(problems, "claude timeout must be positive")
	}
	if c.MaxConcurrency < 1 {
		problems = append(problems, "claude max concurrency must be at least 1")
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// HasOAuthToken reports whether CLAUDE_CODE_OAUTH_TOKEN is set.
func (c ClaudeConfig) HasOAuthToken() bool { return c.OAuthToken != "" }

// AuthEnabled reports whether client API-key authentication is enabled.
func (c ClaudeConfig) AuthEnabled() bool { return len(c.APIKeys) > 0 }

// String never includes secret values, only whether they are set.
func (c ClaudeConfig) String() string {
	return fmt.Sprintf("{Bin:%s Model:%s Timeout:%s MaxConcurrency:%d OAuthTokenSet:%t APIKeys:%d}",
		c.Bin, c.Model, c.Timeout, c.MaxConcurrency, c.HasOAuthToken(), len(c.APIKeys))
}

// GoString keeps %#v from printing secrets.
func (c ClaudeConfig) GoString() string { return "config.ClaudeConfig" + c.String() }

// LogValue implements slog.LogValuer so structured logs never include secrets.
func (c ClaudeConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("bin", c.Bin),
		slog.String("model", c.Model),
		slog.Duration("timeout", c.Timeout),
		slog.Int("max_concurrency", c.MaxConcurrency),
		slog.Bool("oauth_token_set", c.HasOAuthToken()),
		slog.Int("api_keys", len(c.APIKeys)),
	)
}

// parseAPIKeys splits a comma-separated key list, trimming spaces, dropping
// empty entries and silently removing duplicates (order preserved).
func parseAPIKeys(raw string) []string {
	var keys []string
	seen := map[string]struct{}{}
	for _, k := range strings.Split(raw, ",") {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	return keys
}
