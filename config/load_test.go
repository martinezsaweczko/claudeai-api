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

	fs := flag.NewFlagSet("whatsappbot-test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	flag.CommandLine = fs
	os.Args = append([]string{"whatsappbot-test"}, args...)
}

func TestLoadConfig(t *testing.T) {
	publicPath, privatePath := writeKeyPair(t, t.TempDir(), testRSAKey(t))
	keyFlags := []string{"-jwt-public-key-path", publicPath, "-jwt-private-key-path", privatePath}

	t.Run("required flags plus defaults", func(t *testing.T) {
		withCommandLine(t, append(keyFlags,
			"-bot-number", "34936674253",
			"-bot-notification-jid", "34645568517@c.us",
		)...)

		cfg, cfgErr := LoadConfig()
		if cfgErr != nil {
			t.Fatalf("LoadConfig failed: %v", cfgErr)
		}

		if cfg.Bot.BotNumber != "34936674253" || cfg.Bot.ToNotification != "34645568517@c.us" {
			t.Errorf("bot identity not taken from flags: %+v", cfg.Bot)
		}
		if cfg.HttpServer.Port != 46564 || cfg.InternalServer.Port != 9000 {
			t.Errorf("default ports = %d/%d, want 46564/9000", cfg.HttpServer.Port, cfg.InternalServer.Port)
		}
		if cfg.Log.Level != "info" || cfg.Log.FilePath != "stdout" {
			t.Errorf("default log config = %+v", cfg.Log)
		}
		if cfg.O11.PrometheusPath != "/metrics" || cfg.O11.Environment != "dev" || cfg.O11.TracerEndpoint != "" {
			t.Errorf("default o11 config = %+v", cfg.O11)
		}
		if cfg.Bot.DBDriver != "sqlite" || cfg.Bot.DatabaseDSN() != "file:/tmp/db.sqlite?_foreign_keys=on" {
			t.Errorf("default database = %s %s", cfg.Bot.DBDriver, cfg.Bot.DatabaseDSN())
		}
		if cfg.Bot.CommandTimeout != 60 || cfg.Bot.SchedulerTimezone != "Europe/Madrid" || !cfg.Bot.SchedulerEnabled {
			t.Errorf("unexpected bot defaults: %+v", cfg.Bot)
		}
		if cfg.Bot.TrollNumbers != nil {
			t.Errorf("troll numbers should default to nil, got %v", cfg.Bot.TrollNumbers)
		}
		if cfg.JWT.PrivateKey == nil || cfg.JWT.PublicKey == nil {
			t.Error("JWT keys should be loaded")
		}
	})

	t.Run("flags override defaults", func(t *testing.T) {
		withCommandLine(t, append(keyFlags,
			"-bot-number", "34936674253",
			"-bot-notification-jid", "34645568517@c.us",
			"-http-port", "8080",
			"-internal-port", "9100",
			"-http-read-timeout", "7",
			"-log-level", "debug",
			"-bot-troll-numbers", "a@c.us, b@c.us",
			"-scheduler-enabled=false",
			"-db-driver", "mysql",
			"-db-dsn", "user:pass@tcp(db:3306)/bot",
		)...)

		cfg, cfgErr := LoadConfig()
		if cfgErr != nil {
			t.Fatalf("LoadConfig failed: %v", cfgErr)
		}

		if cfg.HttpServer.Port != 8080 || cfg.InternalServer.Port != 9100 {
			t.Errorf("ports = %d/%d, want 8080/9100", cfg.HttpServer.Port, cfg.InternalServer.Port)
		}
		// The internal server has no timeout flags of its own: it shares the public ones.
		if cfg.HttpServer.ReadTimeout != 7 || cfg.InternalServer.ReadTimeout != 7 {
			t.Errorf("read timeouts = %d/%d, want 7/7", cfg.HttpServer.ReadTimeout, cfg.InternalServer.ReadTimeout)
		}
		if cfg.Log.Level != "debug" {
			t.Errorf("log level = %q, want debug", cfg.Log.Level)
		}
		if got := cfg.Bot.TrollNumbers; len(got) != 2 || got[0] != "a@c.us" || got[1] != "b@c.us" {
			t.Errorf("troll numbers = %v, want [a@c.us b@c.us]", got)
		}
		if cfg.Bot.SchedulerEnabled {
			t.Error("scheduler should be disabled by the flag")
		}
		if cfg.Bot.DatabaseDSN() != "user:pass@tcp(db:3306)/bot" {
			t.Errorf("DSN = %q, want the mysql DSN", cfg.Bot.DatabaseDSN())
		}
	})

	t.Run("every invalid section is reported together", func(t *testing.T) {
		// No bot flags, an out-of-range port, a bad log level, an http tracer
		// endpoint and missing key files.
		withCommandLine(t,
			"-http-port", "70000",
			"-log-level", "loud",
			"-o11-tracer-endpoint", "http://tempo:4317",
			"-jwt-public-key-path", "/nonexistent/public.pem",
		)

		cfg, cfgErr := LoadConfig()
		if cfgErr == nil {
			t.Fatal("expected a configuration error")
		}
		if cfg != nil {
			t.Error("no config should be returned alongside an error")
		}
		if len(cfgErr.AppendedErrors) != 5 {
			t.Errorf("got %d appended errors, want 5: %v", len(cfgErr.AppendedErrors), cfgErr)
		}
		for _, want := range []string{
			"server port must be between 1 and 65535",
			"invalid log level: loud",
			"tracer endpoint should not start with http://",
			"JWT public key file not found",
			"bot number cannot be empty",
		} {
			if !strings.Contains(cfgErr.Error(), want) {
				t.Errorf("error should report %q, got:\n%s", want, cfgErr)
			}
		}
	})
}
