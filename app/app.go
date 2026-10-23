package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/martinezsaweczko/claudeai-api/config"
	"github.com/martinezsaweczko/claudeai-api/http/middleware"
	"github.com/martinezsaweczko/claudeai-api/model"
	"github.com/martinezsaweczko/claudeai-api/o11"
	"github.com/martinezsaweczko/claudeai-api/services/claude"
)

// Server defines the interface for any HTTP server implementation
// This interface is defined in the consumer (app) package
type Server interface {
	Start() error
	Stop(ctx context.Context) error
	Handle(pattern string, handler http.Handler)
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
	HandleWithMiddleware(pattern string, handler http.Handler, middlewares ...middleware.Middleware)
	HandleFuncWithMiddleware(pattern string, handler func(http.ResponseWriter, *http.Request), middlewares ...middleware.Middleware)
}

type App struct {
	ctx           context.Context
	config        *config.Config
	server        Server // the single HTTP server: API, health, metrics, Swagger, skills
	log           *slog.Logger
	observability *o11.ObservabilityInst
	buildInfo     model.BuildInfo
	basePath      string
	lookupEnv     func(string) (string, bool)

	// apiKeyAuth guards /v1, /metrics and the skills endpoints. It is a
	// pass-through when no API keys are configured.
	apiKeyAuth middleware.Middleware

	// claude runs chat requests (consumed by the /v1 handlers).
	claude claude.Backend

	// skills keeps the optional skills repository synced; nil when disabled.
	skills               skillsSyncer
	skillsMetrics        *o11.SkillsMetrics // nil when skills are disabled
	skillsStop           func() error
	initialSkillsTimeout time.Duration // test override of initialSkillsSyncTimeout
}

func NewApp(cfg *config.Config) *App {
	return &App{
		config:    cfg,
		ctx:       context.Background(),
		lookupEnv: os.LookupEnv,
	}
}

// WithServer sets the single HTTP server and returns the app instance for chaining.
func (app *App) WithServer(server Server) *App {
	app.server = server
	return app
}

func (app *App) WithLog(log *slog.Logger) *App {
	app.log = log
	return app
}

// WithObservability allows setting up observability providers (tracing, metrics) and returns the app instance for chaining
func (app *App) WithObservability(observability *o11.ObservabilityInst) *App {
	app.observability = observability
	return app
}

// WithBuildInfo sets the build information and returns the app instance for chaining
func (app *App) WithBuildInfo(buildInfo model.BuildInfo) *App {
	app.buildInfo = buildInfo
	return app
}

// WithBasePath sets the base path for all routes and returns the app instance for chaining
func (app *App) WithBasePath(basePath string) *App {
	app.basePath = basePath
	return app
}

func (app *App) Run() error {
	// Validate required dependencies
	if app.server == nil {
		return fmt.Errorf("server dependency is required: use WithServer()")
	}
	if app.log == nil {
		return fmt.Errorf("logger dependency is required: use WithLog()")
	}

	// Wire handlers and routes
	if err := app.initAndWire(); err != nil {
		return fmt.Errorf("failed to initialize: %w", err)
	}

	// Skills: one bounded initial sync so the first requests usually have
	// them, then the background loop. Never blocks startup for long.
	app.startSkills()

	// Start the HTTP server
	if err := app.server.Start(); err != nil {
		return fmt.Errorf("failed to start server: %w", err)
	}

	app.log.Info("Application started",
		"port", app.config.HttpServer.Port,
		"version", app.buildInfo.Version)

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Wait for termination signal
	sig := <-sigChan
	app.log.Info("Received shutdown signal", "signal", sig)

	return app.shutdown()
}

// shutdown gracefully stops the HTTP server
func (app *App) shutdown() error {
	shutdownCtx, cancel := context.WithTimeout(app.ctx, 30*time.Second)
	defer cancel()

	app.log.Info("Starting graceful shutdown")

	var shutdownErr error
	if err := app.server.Stop(shutdownCtx); err != nil {
		app.log.Error("Error during server shutdown", "error", err)
		shutdownErr = err
	}

	if err := app.stopSkills(); err != nil {
		app.log.Error("Error stopping skills sync", "error", err)
		shutdownErr = err
	}
	if err := app.closeBackend(); err != nil {
		app.log.Error("Error closing Claude backend", "error", err)
		shutdownErr = err
	}

	app.log.Info("Application shut down gracefully")
	return shutdownErr
}
