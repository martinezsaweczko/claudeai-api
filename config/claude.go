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

// Web access modes for the Claude CLI (-claude-web).
const (
	WebNone   = "none"   // no web tools (the pre-web behavior)
	WebSearch = "search" // WebSearch only (executed by Anthropic)
	WebFull   = "full"   // WebSearch + WebFetch (WebFetch fetches from this host)
)

// ClaudeConfig configures the Claude Code CLI backend and the optional
// client API-key authentication.
type ClaudeConfig struct {
	Bin            string        // Claude Code CLI binary name or path
	Model          string        // Default model alias when a request names none
	Models         []string      // Model ids advertised by GET /v1/models (includes Model)
	Timeout        time.Duration // Max wall time per request
	MaxConcurrency int           // Max simultaneous CLI processes
	Web            string        // Web tools: none, search or full; empty means none
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
	switch c.Web {
	case "", WebNone, WebSearch, WebFull:
	default:
		problems = append(problems, fmt.Sprintf("claude web must be one of %s, %s, %s (got %q)", WebNone, WebSearch, WebFull, c.Web))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// WebMode returns the effective web mode; an unset value means none.
func (c ClaudeConfig) WebMode() string {
	if c.Web == "" {
		return WebNone
	}
	return c.Web
}

// HasOAuthToken reports whether CLAUDE_CODE_OAUTH_TOKEN is set.
func (c ClaudeConfig) HasOAuthToken() bool { return c.OAuthToken != "" }

// AuthEnabled reports whether client API-key authentication is enabled.
func (c ClaudeConfig) AuthEnabled() bool { return len(c.APIKeys) > 0 }

// String never includes secret values, only whether they are set.
func (c ClaudeConfig) String() string {
	return fmt.Sprintf("{Bin:%s Model:%s Timeout:%s MaxConcurrency:%d Web:%s OAuthTokenSet:%t APIKeys:%d}",
		c.Bin, c.Model, c.Timeout, c.MaxConcurrency, c.WebMode(), c.HasOAuthToken(), len(c.APIKeys))
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
		slog.String("web", c.WebMode()),
		slog.Bool("oauth_token_set", c.HasOAuthToken()),
		slog.Int("api_keys", len(c.APIKeys)),
	)
}

// parseModels splits a comma-separated model list, trimming spaces and
// removing duplicates (order preserved). Empty entries are an error. The default
// model is appended when missing so it is always advertised.
func parseModels(raw, defaultModel string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("claude models cannot be empty")
	}
	var models []string
	seen := map[string]struct{}{}
	add := func(m string) {
		if _, dup := seen[m]; !dup {
			seen[m] = struct{}{}
			models = append(models, m)
		}
	}
	for _, m := range strings.Split(raw, ",") {
		m = strings.TrimSpace(m)
		if m == "" {
			return nil, fmt.Errorf("claude models cannot contain empty entries")
		}
		add(m)
	}
	if d := strings.TrimSpace(defaultModel); d != "" {
		add(d)
	}
	return models, nil
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
