package config

import (
	"strings"
	"testing"
)

func validBotConfig() BotConfig {
	return BotConfig{
		BotNumber:              "34936674253",
		ToNotification:         "34645568517@c.us",
		NacionalFolder:         "/tmp/nacional/",
		RetentionNacional:      24,
		InternacionalFolder:    "/tmp/internacional/",
		RetentionInternacional: 169,
		MagazineFolder:         "/tmp/magazine/",
		RetentionMagazine:      720,
		URLServer:              "localhost:46564",
		DBDriver:               "sqlite",
		DBPath:                 "/tmp/db.sqlite",
		SessionDBPath:          "/tmp/session.db",
		CommandTimeout:         60,
		SchedulerTimezone:      "Europe/Madrid",
		SchedulerEnabled:       true,
		DBAutoCreate:           true,
	}
}

func TestBotConfigValidate(t *testing.T) {
	cfg := validBotConfig()
	if err := cfg.validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestBotConfigValidateErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*BotConfig)
	}{
		{"missing bot number", func(c *BotConfig) { c.BotNumber = "" }},
		{"missing notification jid", func(c *BotConfig) { c.ToNotification = "" }},
		{"missing folder", func(c *BotConfig) { c.NacionalFolder = "" }},
		{"zero retention", func(c *BotConfig) { c.RetentionMagazine = 0 }},
		{"missing url server", func(c *BotConfig) { c.URLServer = "" }},
		{"invalid db driver", func(c *BotConfig) { c.DBDriver = "postgres" }},
		{"missing db path and dsn", func(c *BotConfig) { c.DBPath = ""; c.DBDSN = "" }},
		{"mysql without dsn", func(c *BotConfig) { c.DBDriver = "mysql"; c.DBDSN = ""; c.DBPath = "" }},
		{"missing session db", func(c *BotConfig) { c.SessionDBPath = "" }},
		{"zero timeout", func(c *BotConfig) { c.CommandTimeout = 0 }},
		{"empty scheduler timezone", func(c *BotConfig) { c.SchedulerTimezone = "" }},
		{"invalid scheduler timezone", func(c *BotConfig) { c.SchedulerTimezone = "Mars/Olympus" }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validBotConfig()
			tc.mutate(&cfg)
			if err := cfg.validate(); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestO11ConfigValidate(t *testing.T) {
	valid := &O11Config{TracerEndpoint: "tempo:4317", PrometheusPath: "/metrics"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid o11 config rejected: %v", err)
	}

	badEndpoint := &O11Config{TracerEndpoint: "http://tempo:4317"}
	if err := badEndpoint.Validate(); err == nil {
		t.Error("expected error for http endpoint")
	}

	badPath := &O11Config{PrometheusPath: "metrics"}
	if err := badPath.Validate(); err == nil {
		t.Error("expected error for path without slash")
	}
}

func TestSplitAndTrim(t *testing.T) {
	if got := splitAndTrim(""); got != nil {
		t.Errorf("empty string should give nil, got %v", got)
	}
	got := splitAndTrim("a@c.us, b@g.us ,,  c@c.us  ")
	if len(got) != 3 || got[0] != "a@c.us" || got[1] != "b@g.us" || got[2] != "c@c.us" {
		t.Errorf("unexpected split: %v", got)
	}
}

func TestConfigErrorFormat(t *testing.T) {
	err := &ConfigError{Message: "Invalid configuration", AppendedErrors: []error{
		&ConfigError{Message: "nested one"},
		&ConfigError{Message: "nested two"},
	}}
	msg := err.Error()
	if !strings.Contains(msg, "nested one") || !strings.Contains(msg, "nested two") {
		t.Errorf("ConfigError should list appended errors: %s", msg)
	}
}
