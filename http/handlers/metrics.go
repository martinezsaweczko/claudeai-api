package handlers

import (
	"log/slog"
	"net/http"

	"github.com/martinezsaweczko/whatsappBot-golang/config"
	"github.com/martinezsaweczko/whatsappBot-golang/http/middleware"
	"github.com/martinezsaweczko/whatsappBot-golang/o11"
)

// Handler is an alias for http.Handler
type Handler = http.Handler

// Router defines the interface for registering routes

type MetricsHandlerConfig struct {
	Log           *slog.Logger
	Observability *o11.ObservabilityInst
	Cfg           *config.Config
}

type MetricsHandler struct {
	log           *slog.Logger
	observability *o11.ObservabilityInst
	cfg           *config.Config
}

// NewMetricsHandler creates a new metrics handler
func (m *MetricsHandlerConfig) NewMetricsHandler() *MetricsHandler {
	// This is a wrapper to simplify imports in the app package
	metricsHandler := &MetricsHandler{
		log:           m.Log,
		observability: m.Observability,
		cfg:           m.Cfg,
	}
	return metricsHandler
}

// Prometheus metrics endpoint.
//
//	@Summary      Prometheus metrics
//	@Description  Exposes Prometheus-compatible metrics.
//	@Tags         System
//	@Produce      plain
//	@Success      200 {string} string "Prometheus metrics text"
//	@Router       /metrics [get]
func swaggerMetricsDocs() {}

// Register the routes for the metrics handler
func (m *MetricsHandler) RegisterRoutes(router Router, handler http.Handler, middlewares ...middleware.Middleware) {
	// Register the metrics handler route
	if m.cfg.O11.PrometheusPath != "" {
		if len(middlewares) > 0 {
			router.HandleWithMiddleware(m.cfg.O11.PrometheusPath, handler, middlewares...)
		} else {
			router.Handle(m.cfg.O11.PrometheusPath, handler)
		}
	}
}
