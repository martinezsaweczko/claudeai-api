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

		if cfg.HttpServer.Port != 46564 {
			t.Errorf("default port = %d, want 46564", cfg.HttpServer.Port)
		}
		// Secure by default: the single server listens on loopback unless told
		// otherwise (the container passes 0.0.0.0 explicitly).
		if cfg.HttpServer.Address != "127.0.0.1" {
			t.Errorf("default address = %q, want 127.0.0.1", cfg.HttpServer.Address)
		}
		// Streaming responses need the write timeout disabled by default.
		if cfg.HttpServer.WriteTimeout != 0 {
			t.Errorf("write timeout = %d, want 0 (disabled for SSE)", cfg.HttpServer.WriteTimeout)
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
			"-http-read-timeout", "7",
			"-http-write-timeout", "45",
			"-log-level", "debug",
		)

		cfg, cfgErr := LoadConfig()
		if cfgErr != nil {
			t.Fatalf("LoadConfig failed: %v", cfgErr)
		}

		if cfg.HttpServer.Port != 8080 {
			t.Errorf("port = %d, want 8080", cfg.HttpServer.Port)
		}
		if cfg.HttpServer.ReadTimeout != 7 {
			t.Errorf("read timeout = %d, want 7", cfg.HttpServer.ReadTimeout)
		}
		if cfg.HttpServer.WriteTimeout != 45 {
			t.Errorf("write timeout = %d, want 45", cfg.HttpServer.WriteTimeout)
		}
		if cfg.Log.Level != "debug" {
			t.Errorf("log level = %q, want debug", cfg.Log.Level)
		}
	})

	t.Run("the internal server flags are gone", func(t *testing.T) {
		withCommandLine(t)
		if _, cfgErr := LoadConfig(); cfgErr != nil {
			t.Fatalf("LoadConfig failed: %v", cfgErr)
		}
		for _, name := range []string{"internal-address", "internal-port"} {
			if flag.Lookup(name) != nil {
				t.Errorf("flag -%s is still defined: there is one server and one port", name)
			}
		}
		// The real binary uses ExitOnError, so passing a removed flag aborts
		// startup with "flag provided but not defined"; check that on a fresh set.
		fs := flag.NewFlagSet("claudeai-api-test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		flag.CommandLine = fs
		os.Args = []string{"claudeai-api-test"}
		if _, cfgErr := LoadConfig(); cfgErr != nil {
			t.Fatalf("LoadConfig failed: %v", cfgErr)
		}
		if err := fs.Parse([]string{"-internal-port", "9100"}); err == nil || !strings.Contains(err.Error(), "not defined") {
			t.Errorf("-internal-port parse error = %v, want 'flag provided but not defined'", err)
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
