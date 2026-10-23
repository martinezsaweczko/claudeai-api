package config

import (
	"flag"
	"io"
	"os"
	"strings"
	"testing"
)

// withCommandLine points the process-wide flag set and os.Args at a fresh,
// isolated command line for the duration of one test and restores both
// afterwards. LoadConfig registers its flags on the global flag set, so it can
// only run once per flag set. Tests using this helper must not run in parallel.
func withCommandLine(t *testing.T, args ...string) {
	t.Helper()
	prevFlags, prevArgs := flag.CommandLine, os.Args
	t.Cleanup(func() {
		flag.CommandLine = prevFlags
		os.Args = prevArgs
	})

	fs := flag.NewFlagSet("claudeai-api-test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	flag.CommandLine = fs
	os.Args = append([]string{"claudeai-api-test"}, args...)
}

func TestLoadConfig(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		withCommandLine(t)

		cfg, cfgErr := LoadConfig()
		if cfgErr != nil {
			t.Fatalf("LoadConfig failed: %v", cfgErr)
		}

		if cfg.HttpServer.Port != 46564 || cfg.InternalServer.Port != 9000 {
			t.Errorf("default ports = %d/%d, want 46564/9000", cfg.HttpServer.Port, cfg.InternalServer.Port)
		}
		// Secure by default: both servers listen on loopback unless told otherwise
		// (the container passes 0.0.0.0 explicitly).
		if cfg.HttpServer.Address != "127.0.0.1" || cfg.InternalServer.Address != "127.0.0.1" {
			t.Errorf("default addresses = %q/%q, want 127.0.0.1/127.0.0.1", cfg.HttpServer.Address, cfg.InternalServer.Address)
		}
		// Streaming responses need the public write timeout disabled by default.
		if cfg.HttpServer.WriteTimeout != 0 {
			t.Errorf("public write timeout = %d, want 0 (disabled for SSE)", cfg.HttpServer.WriteTimeout)
		}
		if cfg.InternalServer.WriteTimeout != internalWriteTimeoutSeconds {
			t.Errorf("internal write timeout = %d, want %d", cfg.InternalServer.WriteTimeout, internalWriteTimeoutSeconds)
		}
		if cfg.Log.Level != "info" || cfg.Log.FilePath != "stdout" {
			t.Errorf("default log config = %+v", cfg.Log)
		}
		if cfg.O11.PrometheusPath != "/metrics" || cfg.O11.Environment != "dev" || cfg.O11.TracerEndpoint != "" {
			t.Errorf("default o11 config = %+v", cfg.O11)
		}
	})

	t.Run("flags override defaults", func(t *testing.T) {
		withCommandLine(t,
			"-http-port", "8080",
			"-internal-port", "9100",
			"-http-read-timeout", "7",
			"-http-write-timeout", "45",
			"-log-level", "debug",
		)

		cfg, cfgErr := LoadConfig()
		if cfgErr != nil {
			t.Fatalf("LoadConfig failed: %v", cfgErr)
		}

		if cfg.HttpServer.Port != 8080 || cfg.InternalServer.Port != 9100 {
			t.Errorf("ports = %d/%d, want 8080/9100", cfg.HttpServer.Port, cfg.InternalServer.Port)
		}
		// The internal server has no timeout flags of its own: it shares the public read timeout.
		if cfg.HttpServer.ReadTimeout != 7 || cfg.InternalServer.ReadTimeout != 7 {
			t.Errorf("read timeouts = %d/%d, want 7/7", cfg.HttpServer.ReadTimeout, cfg.InternalServer.ReadTimeout)
		}
		if cfg.HttpServer.WriteTimeout != 45 {
			t.Errorf("public write timeout = %d, want 45", cfg.HttpServer.WriteTimeout)
		}
		if cfg.Log.Level != "debug" {
			t.Errorf("log level = %q, want debug", cfg.Log.Level)
		}
	})

	t.Run("every invalid section is reported together", func(t *testing.T) {
		withCommandLine(t,
			"-http-port", "70000",
			"-log-level", "loud",
			"-o11-tracer-endpoint", "http://tempo:4317",
		)

		cfg, cfgErr := LoadConfig()
		if cfgErr == nil {
			t.Fatal("expected a configuration error")
		}
		if cfg != nil {
			t.Error("no config should be returned alongside an error")
		}
		if len(cfgErr.AppendedErrors) != 3 {
			t.Errorf("got %d appended errors, want 3: %v", len(cfgErr.AppendedErrors), cfgErr)
		}
		for _, want := range []string{
			"server port must be between 1 and 65535",
			"invalid log level: loud",
			"tracer endpoint should not start with http://",
		} {
			if !strings.Contains(cfgErr.Error(), want) {
				t.Errorf("error should report %q, got:\n%s", want, cfgErr)
			}
		}
	})
}
