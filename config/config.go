package config

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	HttpServer ServerConfig // The single HTTP server (API, health, metrics, Swagger, skills)
	Log        LogConfig
	O11        O11Config
	Claude     ClaudeConfig // Claude CLI backend and optional API-key auth
	Skills     SkillsConfig // Optional skills repository synced for the Claude CLI
}

type ConfigError struct {
	Message        string
	AppendedErrors []error
}

// Error implements the error interface for ConfigError
// It formats all validation errors into a single error message
func (ce *ConfigError) Error() string {
	if len(ce.AppendedErrors) == 0 {
		return ce.Message
	}

	var errMessages []string
	for _, err := range ce.AppendedErrors {
		errMessages = append(errMessages, err.Error())
	}

	return fmt.Sprintf("%s:\n  - %s", ce.Message, strings.Join(errMessages, "\n  - "))
}

// LoadConfig loads configuration from command-line flags and the process
// environment (secrets only).
func LoadConfig() (*Config, *ConfigError) {
	return LoadConfigFromEnv(os.LookupEnv)
}

// LoadConfigFromEnv loads configuration from command-line flags and reads
// secrets through lookup (os.LookupEnv in production, a fake in tests).
func LoadConfigFromEnv(lookup func(string) (string, bool)) (*Config, *ConfigError) {
	config := &Config{}
	listErrors := []error{}

	// Define command-line flags for HTTP server configuration (single server)
	var serverAddress string
	var serverPort int
	var serverTimeout int
	var serverReadTimeout int
	var serverWriteTimeout int

	// Define command-line flags for LOG configuration
	var logLevel string
	var logFilePath string

	// Define command-line flags for O11 configuration
	var o11TracerEndpoint string
	var o11PrometheusPath string
	var o11Environment string

	// Define command-line flags for Claude backend configuration
	var claudeBin string
	var claudeModel string
	var claudeModels string
	var claudeTimeout time.Duration
	var claudeMaxConcurrency int
	var claudeWeb string

	// Define command-line flags for the optional skills repository
	var skillsRepo, skillsRef, skillsDir, skillsSSHKey, skillsKnownHosts string
	var skillsRefresh, skillsSyncTimeout time.Duration

	// Flags public API server
	flag.StringVar(&serverAddress, "http-address", "127.0.0.1", "HTTP server bind address (0.0.0.0 listens on every interface; the container image passes it explicitly)")
	flag.IntVar(&serverPort, "http-port", 46564, "HTTP server port (API, health, metrics, Swagger, skills)")
	flag.IntVar(&serverTimeout, "http-timeout", 30, "HTTP server timeout in seconds")
	flag.IntVar(&serverReadTimeout, "http-read-timeout", 10, "HTTP server read timeout in seconds")
	flag.IntVar(&serverWriteTimeout, "http-write-timeout", 0, "Server write timeout in seconds (0 disables it, required for long-lived SSE streaming responses)")

	// Flags log
	flag.StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	flag.StringVar(&logFilePath, "log-file-path", "stdout", "Log file path (default is stdout)")

	// O11 flags
	flag.StringVar(&o11TracerEndpoint, "o11-tracer-endpoint", "", "OpenTelemetry tracer endpoint (gRPC, without http:// or https://)")
	flag.StringVar(&o11PrometheusPath, "o11-prometheus-path", "/metrics", "OpenTelemetry Prometheus metrics path")
	flag.StringVar(&o11Environment, "o11-environment", "dev", "Deployment environment reported in telemetry (dev, prod)")

	// Claude flags (secrets come from CLAUDE_CODE_OAUTH_TOKEN and CLAUDEAI_API_KEYS, not flags)
	flag.StringVar(&claudeBin, "claude-bin", "claude", "Claude Code CLI binary name or path")
	flag.StringVar(&claudeModel, "claude-model", "sonnet", "Default Claude model alias when a request names none")
	flag.StringVar(&claudeModels, "claude-models", "sonnet,opus,haiku", "Comma-separated model ids advertised by GET /v1/models (the default model is added if missing)")
	flag.DurationVar(&claudeTimeout, "claude-timeout", 5*time.Minute, "Max wall time per request")
	flag.IntVar(&claudeMaxConcurrency, "claude-max-concurrency", 4, "Max simultaneous Claude CLI processes")
	flag.StringVar(&claudeWeb, "claude-web", WebSearch, "Claude web tools: none (no web access), search (WebSearch, executed by Anthropic) or full (WebSearch + WebFetch; WebFetch fetches from this host, SSRF risk)")

	// Skills flags (disabled when -skills-repo is empty; paths only, no secrets)
	flag.StringVar(&skillsRepo, "skills-repo", "", "Git URL of the skills repository (ssh, https or file://; no embedded credentials); empty disables skills")
	flag.StringVar(&skillsRef, "skills-ref", "main", "Branch, tag or full commit sha of the skills repository to serve")
	flag.StringVar(&skillsDir, "skills-dir", "", "State directory for the skills clone and versions (default <user cache dir>/claudeai-api/skills)")
	flag.DurationVar(&skillsRefresh, "skills-refresh", 5*time.Minute, "Skills re-sync interval (0 syncs once at startup; minimum 30s otherwise)")
	flag.StringVar(&skillsSSHKey, "skills-ssh-key", "", "Path to the private key file used to clone the skills repository over ssh")
	flag.StringVar(&skillsKnownHosts, "skills-known-hosts", "", "Path to the known_hosts file used to verify the skills git host")
	flag.DurationVar(&skillsSyncTimeout, "skills-sync-timeout", 2*time.Minute, "Max wall time of one skills sync")

	flag.Parse()

	config.HttpServer.Address = serverAddress
	config.HttpServer.Port = serverPort
	config.HttpServer.Timeout = serverTimeout
	config.HttpServer.ReadTimeout = serverReadTimeout
	config.HttpServer.WriteTimeout = serverWriteTimeout

	if err := config.HttpServer.validate(); err != nil {
		listErrors = append(listErrors, err)
	}

	config.Log.Level = logLevel
	config.Log.FilePath = logFilePath

	if err := config.Log.validate(); err != nil {
		listErrors = append(listErrors, err)
	}

	// Set O11 configuration
	o11Conf := &O11Config{
		TracerEndpoint: o11TracerEndpoint,
		PrometheusPath: o11PrometheusPath,
		Environment:    o11Environment,
	}

	config.O11 = *o11Conf

	if err := o11Conf.Validate(); err != nil {
		listErrors = append(listErrors, err)
	}

	config.Claude = ClaudeConfig{
		Bin:            claudeBin,
		Model:          claudeModel,
		Timeout:        claudeTimeout,
		MaxConcurrency: claudeMaxConcurrency,
		Web:            strings.TrimSpace(claudeWeb),
	}
	if models, err := parseModels(claudeModels, claudeModel); err != nil {
		listErrors = append(listErrors, err)
	} else {
		config.Claude.Models = models
	}
	if v, ok := lookup(envOAuthToken); ok {
		config.Claude.OAuthToken = strings.TrimSpace(v)
	}
	if v, ok := lookup(envAPIKeys); ok {
		config.Claude.APIKeys = parseAPIKeys(v)
	}

	if err := config.Claude.validate(); err != nil {
		listErrors = append(listErrors, err)
	}

	config.Skills = SkillsConfig{
		Repo:        strings.TrimSpace(skillsRepo),
		Ref:         strings.TrimSpace(skillsRef),
		Dir:         strings.TrimSpace(skillsDir),
		Refresh:     skillsRefresh,
		SSHKey:      strings.TrimSpace(skillsSSHKey),
		KnownHosts:  strings.TrimSpace(skillsKnownHosts),
		SyncTimeout: skillsSyncTimeout,
	}
	if config.Skills.Enabled() && config.Skills.Dir == "" {
		if dir, err := defaultSkillsDir(); err != nil {
			listErrors = append(listErrors, err)
		} else {
			config.Skills.Dir = dir
		}
	}
	if err := config.Skills.validate(); err != nil {
		listErrors = append(listErrors, err)
	}

	if len(listErrors) > 0 {
		return nil, &ConfigError{Message: "Invalid configuration", AppendedErrors: listErrors}
	}

	return config, nil
}
