package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/martinezsaweczko/claudeai-api/config"
)

func TestIsLoopbackAddress(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.1.2.3", true},
		{"::1", true},
		{"localhost", true},
		{"LOCALHOST", true},
		{"0.0.0.0", false},
		{"::", false},
		{"", false},
		{"192.168.1.10", false},
		{"example.com", false},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			if got := isLoopbackAddress(tt.addr); got != tt.want {
				t.Errorf("isLoopbackAddress(%q) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

func TestLogClaudeStartup(t *testing.T) {
	const secretKey, secretToken = "key-SECRET", "tok-SECRET"
	base := config.ClaudeConfig{Bin: "claude", Model: "sonnet", Timeout: time.Minute, MaxConcurrency: 1}

	tests := []struct {
		name      string
		address   string
		keys      []string
		token     string
		env       map[string]string
		want      []string
		wantAbsnt []string
	}{
		{
			name:    "auth disabled on a public address warns",
			address: "0.0.0.0",
			want:    []string{"api_key_auth=disabled", "api_keys=0", "oauth_token_set=false", "level=WARN", "unauthenticated"},
		},
		{
			name:      "auth disabled on loopback does not warn",
			address:   "127.0.0.1",
			want:      []string{"api_key_auth=disabled"},
			wantAbsnt: []string{"level=WARN"},
		},
		{
			name:      "auth enabled on a public address does not warn",
			address:   "0.0.0.0",
			keys:      []string{secretKey, "other"},
			token:     secretToken,
			want:      []string{"api_key_auth=enabled", "api_keys=2", "oauth_token_set=true"},
			wantAbsnt: []string{"level=WARN", secretKey, secretToken},
		},
		{
			name:      "ANTHROPIC_API_KEY set warns about billing",
			address:   "127.0.0.1",
			env:       map[string]string{"ANTHROPIC_API_KEY": "sk-ant-SECRET"},
			want:      []string{"level=WARN", "ANTHROPIC_API_KEY"},
			wantAbsnt: []string{"sk-ant-SECRET"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, nil))
			cfg := &config.Config{Claude: base}
			cfg.HttpServer.Address = tt.address
			cfg.Claude.APIKeys = tt.keys
			cfg.Claude.OAuthToken = tt.token
			lookup := func(k string) (string, bool) { v, ok := tt.env[k]; return v, ok }

			logClaudeStartup(log, cfg, lookup)

			out := buf.String()
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("log should contain %q, got:\n%s", w, out)
				}
			}
			for _, w := range tt.wantAbsnt {
				if strings.Contains(out, w) {
					t.Errorf("log must not contain %q, got:\n%s", w, out)
				}
			}
		})
	}
}
