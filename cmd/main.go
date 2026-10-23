package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/gofrs/uuid"

	"github.com/martinezsaweczko/claudeai-api/app"
	"github.com/martinezsaweczko/claudeai-api/config"
	"github.com/martinezsaweczko/claudeai-api/http_server"
	"github.com/martinezsaweczko/claudeai-api/model"
	"github.com/martinezsaweczko/claudeai-api/o11"
)

// Buildinfo variables set via LDFlags in the Makefile
var (
	version   string
	buildTime string
	appName   string
)

// @title Claude OpenAI-compatible API
// @version 1.0.0
// @description OpenAI-compatible HTTP API backed by the Claude Code CLI. One server, one port: /v1/*, /metrics and /api/v1/skills* require the API key when CLAUDEAI_API_KEYS is set; /api/v1/health, /api/v1/version and this documentation are always public.
//
// @license.name MIT
//
// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization
// @description "Bearer <key>". Only enforced when CLAUDEAI_API_KEYS is set.
func main() {
	// Health check mode for container HEALTHCHECK (slim images have no curl):
	// claudeai-api -health-check [port]
	if len(os.Args) > 1 && os.Args[1] == "-health-check" {
		os.Exit(healthCheck())
	}

	// Create context for the application
	ctx := context.Background()

	// Create an instance of the application
	cfg, err := config.LoadConfig()
	if err != nil {
		fmt.Printf("Configuration error: %v\n", err)
		os.Exit(1)
	}

	// Create logger with config options, wrapped with trace correlation
	file, errFile := config.LogFilePath(cfg.Log.FilePath)
	if errFile != nil {
		fmt.Printf("Error opening log file: %v\n", errFile)
		os.Exit(1)
	}
	defer file.Close()

	logLevel := config.ParseLogLevel(cfg.Log.Level)
	baseHandler := slog.NewTextHandler(file, &slog.HandlerOptions{Level: logLevel})
	log := slog.New(o11.NewTraceLogHandler(baseHandler))

	// Group execution UUID for all logs in this run
	executionID, _ := uuid.NewV4()
	log = log.With("execution_id", executionID)

	// Print build info
	log.Info("Build Info information", "version", version, "build_time", buildTime, "app_name", appName)

	// Set up OpenTelemetry providers.
	otelShutdown, observabilityInst, otelErr := o11.SetupOTelSDK(ctx, &cfg.O11, "claudeai-api", version)
	if otelErr != nil {
		log.Error("OpenTelemetry setup error", "error", otelErr)
		os.Exit(1)
	}
	defer func() {
		if err := otelShutdown(ctx); err != nil {
			log.Error("OpenTelemetry shutdown error", "error", err)
		}
	}()

	// The single HTTP server (API, health, metrics, Swagger, skills)
	server := (&http_server.HTTPServerConfig{
		Addr:         cfg.HttpServer.Address,
		Port:         cfg.HttpServer.Port,
		Timeout:      cfg.HttpServer.Timeout,
		ReadTimeout:  cfg.HttpServer.ReadTimeout,
		WriteTimeout: cfg.HttpServer.WriteTimeout,
		Log:          log,
	}).New()

	// Build app with all dependencies injected
	application := app.NewApp(cfg).
		WithServer(server).
		WithLog(log).
		WithObservability(&observabilityInst).
		WithBuildInfo(model.BuildInfo{
			Version:   version,
			BuildTime: buildTime,
			AppName:   appName,
		}).
		WithBasePath("/api/v1")

	// Run the application
	if err := application.Run(); err != nil {
		log.Error("Application error", "error", err)
		os.Exit(1)
	}

	log.Info("Application terminated")
}

// defaultHealthPort is the single server's default port (-http-port).
const defaultHealthPort = "46564"

// healthCheck queries the local health endpoint (public, no API key needed)
// and returns 0 on success. It connects over loopback, which works for a server
// bound to 127.0.0.1 or 0.0.0.0.
func healthCheck() int {
	port := defaultHealthPort
	if len(os.Args) > 2 {
		port = os.Args[2]
	}
	return probeHealth("http://127.0.0.1:" + port + "/api/v1/health")
}

// probeHealth GETs url and returns 0 only for a 200 answer.
func probeHealth(url string) int {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "health check failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "health check returned status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
