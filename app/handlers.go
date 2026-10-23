package app

import (
	"fmt"

	"github.com/martinezsaweczko/claudeai-api/http/handlers"
	"github.com/martinezsaweczko/claudeai-api/http/middleware"
	"github.com/martinezsaweczko/claudeai-api/services/claude"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// initAndWire creates the handlers and registers the HTTP routes.
func (app *App) initAndWire() error {
	cfg := app.config
	log := app.log

	// ----- HTTP handlers -----
	healthHandlerConf := handlers.HealthHandlerConfig{
		Log:           log,
		Observability: app.observability,
		Cfg:           cfg,
		BasePath:      app.basePath,
	}
	healthHandler, err := healthHandlerConf.NewHealthHandler()
	if err != nil {
		return fmt.Errorf("failed to create health handler: %w", err)
	}

	versionHandlerConf := handlers.VersionHandlerConfig{
		Log:           log,
		Observability: app.observability,
		Cfg:           cfg,
		AppBuildInfo:  app.buildInfo,
		BasePath:      app.basePath,
	}
	versionHandler, err := versionHandlerConf.NewVersionHandler()
	if err != nil {
		return fmt.Errorf("failed to create version handler: %w", err)
	}

	metricsHandlerConf := handlers.MetricsHandlerConfig{
		Log:           log,
		Observability: app.observability,
		Cfg:           cfg,
	}
	metricsHandler := metricsHandlerConf.NewMetricsHandler()

	swaggerHandler := handlers.NewSwaggerHandler(log, app.basePath)

	// ----- Claude backend security posture -----
	app.apiKeyAuth = middleware.APIKeyAuth(cfg.Claude.APIKeys)
	logClaudeStartup(log, cfg, app.lookupEnv)

	claudeOpts := []claude.Option{claude.WithLogger(log)}
	if app.observability != nil && app.observability.Trace != nil {
		claudeOpts = append(claudeOpts, claude.WithTracerProvider(app.observability.Trace))
	}
	app.claude = claude.NewCLIBackend(cfg.Claude, claudeOpts...)

	// ----- Routes -----
	loggingMiddleware := middleware.LoggingMiddleware(log)

	// Internal server: health, version, metrics, swagger
	healthHandler.RegisterRoutes(app.internalServer)
	versionHandler.RegisterRoutes(app.internalServer, loggingMiddleware)
	if cfg.O11.PrometheusPath != "" {
		metricsHandler.RegisterRoutes(app.internalServer, promhttp.Handler())
	}
	swaggerHandler.RegisterRoutes(app.internalServer, loggingMiddleware)

	log.Info("Application wired successfully")

	return nil
}
