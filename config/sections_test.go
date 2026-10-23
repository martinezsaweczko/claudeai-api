package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogConfigValidate(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name    string
		cfg     LogConfig
		wantErr string
	}{
		{"debug to stdout", LogConfig{Level: "debug", FilePath: "stdout"}, ""},
		{"info", LogConfig{Level: "info"}, ""},
		{"warn", LogConfig{Level: "warn"}, ""},
		{"error", LogConfig{Level: "error"}, ""},
		{"file in an existing directory", LogConfig{Level: "info", FilePath: filepath.Join(dir, "app.log")}, ""},
		{"blank file path is ignored", LogConfig{Level: "info", FilePath: "   "}, ""},
		{"unknown level", LogConfig{Level: "trace"}, "invalid log level: trace"},
		{"level is case sensitive", LogConfig{Level: "INFO"}, "invalid log level: INFO"},
		{"empty level", LogConfig{}, "invalid log level"},
		{"file in a missing directory", LogConfig{Level: "info", FilePath: filepath.Join(dir, "nope", "app.log")}, "log file directory does not exist"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validate()
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

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		level string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},
		{"verbose", slog.LevelInfo},
	}
	for _, tt := range tests {
		t.Run("level "+tt.level, func(t *testing.T) {
			if got := ParseLogLevel(tt.level); got != tt.want {
				t.Errorf("ParseLogLevel(%q) = %v, want %v", tt.level, got, tt.want)
			}
		})
	}
}

func TestLogFilePath(t *testing.T) {
	t.Run("stdout keyword returns os.Stdout", func(t *testing.T) {
		f, err := LogFilePath("stdout")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if f != os.Stdout {
			t.Error("expected os.Stdout")
		}
	})

	t.Run("creates the file and appends to it", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "app.log")
		for _, line := range []string{"first\n", "second\n"} {
			f, err := LogFilePath(path)
			if err != nil {
				t.Fatalf("LogFilePath failed: %v", err)
			}
			if _, err := f.WriteString(line); err != nil {
				t.Fatalf("write failed: %v", err)
			}
			if err := f.Close(); err != nil {
				t.Fatalf("close failed: %v", err)
			}
		}

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read failed: %v", err)
		}
		if string(got) != "first\nsecond\n" {
			t.Errorf("file content = %q, want both lines appended", got)
		}
	})

	t.Run("missing directory is an error", func(t *testing.T) {
		if _, err := LogFilePath(filepath.Join(t.TempDir(), "nope", "app.log")); err == nil {
			t.Fatal("expected an error for a missing directory")
		}
	})
}

func TestServerConfigValidate(t *testing.T) {
	valid := ServerConfig{Address: "0.0.0.0", Port: 46564, Timeout: 30, ReadTimeout: 10, WriteTimeout: 30}

	tests := []struct {
		name    string
		mutate  func(*ServerConfig)
		wantErr string
	}{
		{"valid", func(*ServerConfig) {}, ""},
		{"zero timeouts are allowed", func(c *ServerConfig) { c.Timeout, c.ReadTimeout, c.WriteTimeout = 0, 0, 0 }, ""},
		{"lowest port", func(c *ServerConfig) { c.Port = 1 }, ""},
		{"highest port", func(c *ServerConfig) { c.Port = 65535 }, ""},
		{"empty address", func(c *ServerConfig) { c.Address = "" }, "address cannot be empty"},
		{"port zero", func(c *ServerConfig) { c.Port = 0 }, "port must be between 1 and 65535"},
		{"port above range", func(c *ServerConfig) { c.Port = 65536 }, "port must be between 1 and 65535"},
		{"negative timeout", func(c *ServerConfig) { c.Timeout = -1 }, "server timeout cannot be negative"},
		{"negative read timeout", func(c *ServerConfig) { c.ReadTimeout = -1 }, "read timeout cannot be negative"},
		{"negative write timeout", func(c *ServerConfig) { c.WriteTimeout = -1 }, "write timeout cannot be negative"},
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

func TestConfigErrorWithoutAppendedErrors(t *testing.T) {
	err := &ConfigError{Message: "Invalid configuration"}
	if got := err.Error(); got != "Invalid configuration" {
		t.Errorf("Error() = %q, want the bare message", got)
	}
}
