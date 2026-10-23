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
// @description OpenAI-compatible HTTP API backed by the Claude Code CLI.
//
// @license.name MIT
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

	// HTTP servers
	publicServer := (&http_server.HTTPServerConfig{
		Addr:         cfg.HttpServer.Address,
		Port:         cfg.HttpServer.Port,
		Timeout:      cfg.HttpServer.Timeout,
		ReadTimeout:  cfg.HttpServer.ReadTimeout,
		WriteTimeout: cfg.HttpServer.WriteTimeout,
		Log:          log,
	}).New()

	internalServer := (&http_server.HTTPServerConfig{
		Addr:         cfg.InternalServer.Address,
		Port:         cfg.InternalServer.Port,
		Timeout:      cfg.InternalServer.Timeout,
		ReadTimeout:  cfg.InternalServer.ReadTimeout,
		WriteTimeout: cfg.InternalServer.WriteTimeout,
		Log:          log,
	}).New()

	// Build app with all dependencies injected
	application := app.NewApp(cfg).
		WithPublicServer(publicServer).
		WithInternalServer(internalServer).
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

// healthCheck queries the internal server health endpoint and returns 0 on success
func healthCheck() int {
	port := "9000"
	if len(os.Args) > 2 {
		port = os.Args[2]
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://localhost:" + port + "/api/v1/health")
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
