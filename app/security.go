package app

import (
	"log/slog"
	"net"
	"strings"

	"github.com/martinezsaweczko/claudeai-api/config"
)

// logClaudeStartup reports the auth and credential posture at startup. It
// never logs secret values, only booleans and counts.
func logClaudeStartup(log *slog.Logger, cfg *config.Config, lookupEnv func(string) (string, bool)) {
	c := cfg.Claude
	state := "disabled"
	if c.AuthEnabled() {
		state = "enabled"
	}
	log.Info("Claude backend configuration",
		"api_key_auth", state,
		"api_keys", len(c.APIKeys),
		"oauth_token_set", c.HasOAuthToken(),
		"claude_bin", c.Bin,
		"model", c.Model,
		"max_concurrency", c.MaxConcurrency,
		"timeout", c.Timeout,
		"claude_web", c.WebMode())

	if c.WebMode() == config.WebFull {
		log.Warn("claude_web=full enables WebFetch: the Claude CLI fetches pages from THIS host, so a prompt-injected or malicious request can make the gateway reach internal addresses (SSRF); the CLI refuses localhost and dotless hostnames but its handling of private IPs is not documented. Prefer claude_web=search unless the host network is trusted")
	}

	if !c.AuthEnabled() && !isLoopbackAddress(cfg.HttpServer.Address) {
		log.Warn("The server is unauthenticated and reachable beyond localhost: the API, /metrics and skills refresh are all open on this single port; set CLAUDEAI_API_KEYS, bind -http-address to 127.0.0.1, or (in a container) publish the port on 127.0.0.1 only",
			"http_address", cfg.HttpServer.Address)
	}

	if v, ok := lookupEnv("ANTHROPIC_API_KEY"); ok && v != "" {
		log.Warn("ANTHROPIC_API_KEY is set: it overrides the Claude subscription login and would bill the API key; it will be stripped from Claude child processes")
	}
}

// isLoopbackAddress reports whether a bind address only accepts local
// connections. Empty and unspecified addresses (0.0.0.0, ::) listen on every
// interface and are not loopback.
func isLoopbackAddress(addr string) bool {
	if strings.EqualFold(addr, "localhost") {
		return true
	}
	ip := net.ParseIP(addr)
	return ip != nil && ip.IsLoopback()
}
