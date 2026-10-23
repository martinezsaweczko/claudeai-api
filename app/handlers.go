package app

import (
	"fmt"

	"github.com/martinezsaweczko/claudeai-api/config"
	"github.com/martinezsaweczko/claudeai-api/http/handlers"
	"github.com/martinezsaweczko/claudeai-api/http/middleware"
	"github.com/martinezsaweczko/claudeai-api/o11"
	"github.com/martinezsaweczko/claudeai-api/services/claude"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// openAIBasePath is where OpenAI clients mount the API (base URL http://host:port/v1).
const openAIBasePath = "/v1"

// advertisedModels returns the configured model list, falling back to the
// default model when none is configured.
func advertisedModels(c config.ClaudeConfig) []string {
	if len(c.Models) > 0 {
		return c.Models
	}
	return []string{c.Model}
}

// initAndWire creates the handlers and registers the HTTP routes.
func (app *App) initAndWire() error {
	cfg := app.config
	log := app.log

	if app.observability == nil || app.observability.MeterProvider == nil {
		return fmt.Errorf("observability with a meter provider is required (call WithObservability)")
	}

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

	skillsHandlerConf := handlers.SkillsHandlerConfig{
		Log:      log,
		Repo:     cfg.Skills.Repo,
		Ref:      cfg.Skills.Ref,
		BasePath: app.basePath,
	}

	// ----- Claude backend security posture -----
	app.apiKeyAuth = middleware.APIKeyAuth(cfg.Claude.APIKeys)
	logClaudeStartup(log, cfg, app.lookupEnv)

	// ----- Claude and skills metrics -----
	meterProvider := app.observability.MeterProvider
	claudeMetrics, err := o11.NewClaudeMetrics(meterProvider)
	if err != nil {
		return fmt.Errorf("failed to create Claude metrics: %w", err)
	}
	if cfg.Skills.Enabled() || app.skills != nil {
		if app.skillsMetrics, err = o11.NewSkillsMetrics(meterProvider); err != nil {
			return fmt.Errorf("failed to create skills metrics: %w", err)
		}
	}

	logSkillsStartup(log, cfg.Skills)
	if err := app.newSkillsSyncer(); err != nil {
		return err
	}
	skillNames := func() []string { return nil }
	if app.skills != nil {
		skillNames = currentSkillNames(app.skills)
		if err := app.skillsMetrics.RegisterState(skillsStateFunc(app.skills)); err != nil {
			return fmt.Errorf("failed to register skills metrics: %w", err)
		}
	}

	claudeOpts := []claude.Option{
		claude.WithLogger(log),
		claude.WithObserver(claudeRunObserver(claudeMetrics, newModelNormalizer(cfg.Claude), skillNames)),
	}
	if app.skills != nil {
		claudeOpts = append(claudeOpts, claude.WithSkills(skillsProvider(app.skills)))
	}
	if app.observability != nil && app.observability.Trace != nil {
		claudeOpts = append(claudeOpts, claude.WithTracerProvider(app.observability.Trace))
	}
	if app.claude == nil {
		app.claude = claude.NewCLIBackend(cfg.Claude, claudeOpts...)
	}
	if be, ok := app.claude.(interface{ InFlight() int64 }); ok {
		if err := claudeMetrics.RegisterInFlight(be.InFlight); err != nil {
			return fmt.Errorf("failed to register Claude metrics: %w", err)
		}
	}

	// ----- OpenAI-compatible API (public server, base path /v1) -----
	httpMetrics, err := o11.NewHTTPMetrics(app.observability.MeterProvider)
	if err != nil {
		return fmt.Errorf("failed to create HTTP metrics: %w", err)
	}
	chatHandler := (&handlers.ChatHandlerConfig{
		Log:          log,
		Backend:      app.claude,
		DefaultModel: cfg.Claude.Model,
		BasePath:     openAIBasePath,
	}).NewChatHandler()
	modelsHandler := (&handlers.ModelsHandlerConfig{
		Log:      log,
		Models:   advertisedModels(cfg.Claude),
		BasePath: openAIBasePath,
	}).NewModelsHandler()

	// ----- Routes -----
	loggingMiddleware := middleware.LoggingMiddleware(log)

	// Internal server: health, version, metrics, swagger
	healthHandler.RegisterRoutes(app.internalServer)
	versionHandler.RegisterRoutes(app.internalServer, loggingMiddleware)
	if cfg.O11.PrometheusPath != "" {
		metricsHandler.RegisterRoutes(app.internalServer, promhttp.Handler())
	}
	swaggerHandler.RegisterRoutes(app.internalServer, loggingMiddleware)
	// Skills status/refresh: unauthenticated like health and metrics, so the
	// internal port must never be exposed publicly.
	skillsHandler := skillsHandlerConf
	if app.skills != nil {
		skillsHandler.Source = app.skills
	}
	skillsHandler.NewSkillsHandler().RegisterRoutes(app.internalServer,
		middleware.TracingMiddleware(),
		middleware.MetricsMiddleware(httpMetrics),
		loggingMiddleware,
	)

	// Public server: /v1 behind tracing -> metrics -> logging -> API-key auth.
	v1Middlewares := []middleware.Middleware{
		middleware.TracingMiddleware(),
		middleware.MetricsMiddleware(httpMetrics),
		loggingMiddleware,
		app.apiKeyAuth,
	}
	modelsHandler.RegisterRoutes(app.publicServer, v1Middlewares...)
	chatHandler.RegisterRoutes(app.publicServer, v1Middlewares...)

	log.Info("Application wired successfully")

	return nil
}
