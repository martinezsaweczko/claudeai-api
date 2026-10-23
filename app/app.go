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

	"github.com/martinezsaweczko/whatsappBot-golang/config"
	"github.com/martinezsaweczko/whatsappBot-golang/http/middleware"
	"github.com/martinezsaweczko/whatsappBot-golang/model"
	"github.com/martinezsaweczko/whatsappBot-golang/o11"
	"github.com/martinezsaweczko/whatsappBot-golang/repository"
	"github.com/martinezsaweczko/whatsappBot-golang/services/scheduler"
	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
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
	ctx            context.Context
	config         *config.Config
	publicServer   Server
	internalServer Server
	waClient       *whatsapp.Client
	router         *whatsapp.Router
	repo           *repository.DB
	log            *slog.Logger
	observability  *o11.ObservabilityInst
	buildInfo      model.BuildInfo
	basePath       string
	scheduler      *scheduler.Service
}

func NewApp(cfg *config.Config) *App {
	return &App{
		config: cfg,
		ctx:    context.Background(),
	}
}

func (app *App) WithPublicServer(server Server) *App {
	app.publicServer = server
	return app
}

func (app *App) WithInternalServer(server Server) *App {
	app.internalServer = server
	return app
}

func (app *App) WithWhatsApp(client *whatsapp.Client, router *whatsapp.Router) *App {
	app.waClient = client
	app.router = router
	return app
}

func (app *App) WithRepository(repo *repository.DB) *App {
	app.repo = repo
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

// WithScheduler sets the scheduler service and returns the app instance for chaining.
func (app *App) WithScheduler(s *scheduler.Service) *App {
	app.scheduler = s
	return app
}

func (app *App) Run() error {
	// Validate required dependencies
	if app.publicServer == nil || app.internalServer == nil {
		return fmt.Errorf("server dependencies are required: use WithPublicServer() and WithInternalServer()")
	}
	if app.waClient == nil || app.router == nil {
		return fmt.Errorf("whatsapp dependencies are required: use WithWhatsApp()")
	}
	if app.repo == nil {
		return fmt.Errorf("repository dependency is required: use WithRepository()")
	}
	if app.log == nil {
		return fmt.Errorf("logger dependency is required: use WithLog()")
	}

	// Wire handlers, routes and commands
	if err := app.initAndWire(); err != nil {
		return fmt.Errorf("failed to initialize: %w", err)
	}

	// Start HTTP servers
	if err := app.publicServer.Start(); err != nil {
		return fmt.Errorf("failed to start public server: %w", err)
	}
	if err := app.internalServer.Start(); err != nil {
		return fmt.Errorf("failed to start internal server: %w", err)
	}

	// Connect to WhatsApp (login flow if needed)
	if err := app.waClient.Connect(app.ctx, app.config.Bot.PairPhone); err != nil {
		return fmt.Errorf("failed to connect to WhatsApp: %w", err)
	}

	// Start the scheduler after WhatsApp is connected so jobs can send messages.
	if app.scheduler != nil {
		if err := app.scheduler.Start(app.ctx); err != nil {
			return fmt.Errorf("failed to start scheduler: %w", err)
		}
	}

	app.log.Info("Application started",
		"public_port", app.config.HttpServer.Port,
		"internal_port", app.config.InternalServer.Port,
		"version", app.buildInfo.Version)

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Wait for termination signal
	sig := <-sigChan
	app.log.Info("Received shutdown signal", "signal", sig)

	return app.shutdown()
}

// shutdown stops servers, waits for in-flight commands and disconnects WhatsApp
func (app *App) shutdown() error {
	shutdownCtx, cancel := context.WithTimeout(app.ctx, 30*time.Second)
	defer cancel()

	app.log.Info("Starting graceful shutdown")

	var shutdownErr error
	if err := app.publicServer.Stop(shutdownCtx); err != nil {
		app.log.Error("Error during public server shutdown", "error", err)
		shutdownErr = err
	}
	if err := app.internalServer.Stop(shutdownCtx); err != nil {
		app.log.Error("Error during internal server shutdown", "error", err)
		shutdownErr = err
	}

	// Stop the scheduler before waiting for in-flight commands.
	if app.scheduler != nil {
		app.scheduler.Stop()
	}

	// Wait for in-flight WhatsApp commands, then disconnect
	app.log.Info("Waiting for in-flight commands to finish")
	app.router.Wait()
	app.waClient.Disconnect()

	app.log.Info("Application shut down gracefully")
	return shutdownErr
}
