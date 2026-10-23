package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeEnv returns an injectable environment lookup backed by a map.
func fakeEnv(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func TestClaudeConfigValidate(t *testing.T) {
	valid := ClaudeConfig{Bin: "claude", Model: "sonnet", Timeout: 5 * time.Minute, MaxConcurrency: 4}

	tests := []struct {
		name    string
		mutate  func(*ClaudeConfig)
		wantErr string
	}{
		{"valid", func(*ClaudeConfig) {}, ""},
		{"concurrency of one", func(c *ClaudeConfig) { c.MaxConcurrency = 1 }, ""},
		{"empty bin", func(c *ClaudeConfig) { c.Bin = " " }, "claude binary cannot be empty"},
		{"empty model", func(c *ClaudeConfig) { c.Model = "" }, "claude model cannot be empty"},
		{"zero timeout", func(c *ClaudeConfig) { c.Timeout = 0 }, "claude timeout must be positive"},
		{"negative timeout", func(c *ClaudeConfig) { c.Timeout = -time.Second }, "claude timeout must be positive"},
		{"zero concurrency", func(c *ClaudeConfig) { c.MaxConcurrency = 0 }, "claude max concurrency must be at least 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)
			err := cfg.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseAPIKeys(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"empty", "", nil},
		{"only separators and spaces", " , ,, ", nil},
		{"single", "k1", []string{"k1"}},
		{"trimmed list", " k1 , k2,k3 ", []string{"k1", "k2", "k3"}},
		{"empty entries dropped", "k1,,k2,", []string{"k1", "k2"}},
		{"duplicates removed keeping order", "k2,k1,k2, k1", []string{"k2", "k1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseAPIKeys(tt.raw); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseAPIKeys(%q) = %#v, want %#v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestLoadConfigClaude(t *testing.T) {
	t.Run("defaults and no secrets", func(t *testing.T) {
		withCommandLine(t)

		cfg, cfgErr := LoadConfigFromEnv(fakeEnv(nil))
		if cfgErr != nil {
			t.Fatalf("LoadConfigFromEnv failed: %v", cfgErr)
		}
		c := cfg.Claude
		if c.Bin != "claude" || c.Model != "sonnet" || c.Timeout != 5*time.Minute || c.MaxConcurrency != 4 {
			t.Errorf("default claude config = %+v", c)
		}
		if c.HasOAuthToken() || c.AuthEnabled() || len(c.APIKeys) != 0 {
			t.Errorf("secrets should be absent by default: %+v", c)
		}
	})

	t.Run("flags and environment", func(t *testing.T) {
		withCommandLine(t,
			"-claude-bin", "/opt/claude",
			"-claude-model", "opus",
			"-claude-timeout", "90s",
			"-claude-max-concurrency", "2",
		)
		env := fakeEnv(map[string]string{
			"CLAUDE_CODE_OAUTH_TOKEN": "  oauth-secret  ",
			"CLAUDEAI_API_KEYS":       "key-a, key-b,key-a",
		})

		cfg, cfgErr := LoadConfigFromEnv(env)
		if cfgErr != nil {
			t.Fatalf("LoadConfigFromEnv failed: %v", cfgErr)
		}
		c := cfg.Claude
		if c.Bin != "/opt/claude" || c.Model != "opus" || c.Timeout != 90*time.Second || c.MaxConcurrency != 2 {
			t.Errorf("claude config = %+v", c)
		}
		if !c.HasOAuthToken() || c.OAuthToken != "oauth-secret" {
			t.Errorf("oauth token not read from the environment")
		}
		if !c.AuthEnabled() || !reflect.DeepEqual(c.APIKeys, []string{"key-a", "key-b"}) {
			t.Errorf("api keys = %v, want [key-a key-b]", c.APIKeys)
		}
	})

	t.Run("invalid claude flags are reported with the rest", func(t *testing.T) {
		withCommandLine(t, "-claude-max-concurrency", "0", "-claude-timeout", "0s", "-http-port", "0")

		_, cfgErr := LoadConfigFromEnv(fakeEnv(nil))
		if cfgErr == nil {
			t.Fatal("expected a configuration error")
		}
		for _, want := range []string{
			"claude max concurrency must be at least 1",
			"claude timeout must be positive",
			"server port must be between 1 and 65535",
		} {
			if !strings.Contains(cfgErr.Error(), want) {
				t.Errorf("error should report %q, got:\n%s", want, cfgErr)
			}
		}
	})
}

func TestClaudeConfigNeverPrintsSecrets(t *testing.T) {
	const token, keyA, keyB = "oauth-SECRET-token", "api-SECRET-a", "api-SECRET-b"
	claude := ClaudeConfig{
		Bin: "claude", Model: "sonnet", Timeout: time.Minute, MaxConcurrency: 1,
		OAuthToken: token, APIKeys: []string{keyA, keyB},
	}
	full := Config{Claude: claude}

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	log.Info("claude", "cfg", claude, "ptr", &claude, "full", full)
	jsonBuf := &bytes.Buffer{}
	slog.New(slog.NewJSONHandler(jsonBuf, nil)).Info("claude", "cfg", claude, "full", full)

	outputs := map[string]string{
		"text handler": buf.String(),
		"json handler": jsonBuf.String(),
		"String":       claude.String(),
		"%v":           fmt.Sprintf("%v", claude),
		"%+v":          fmt.Sprintf("%+v", full),
		"%#v":          fmt.Sprintf("%#v", claude),
	}
	for name, out := range outputs {
		for _, secret := range []string{token, keyA, keyB} {
			if strings.Contains(out, secret) {
				t.Errorf("%s leaked a secret: %s", name, out)
			}
		}
	}
	if out := buf.String(); !strings.Contains(out, "sonnet") {
		t.Errorf("non-secret fields should still be logged, got: %s", out)
	}
}
