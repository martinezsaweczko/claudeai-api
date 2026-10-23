package handlers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/martinezsaweczko/claudeai-api/config"
	"github.com/martinezsaweczko/claudeai-api/http/middleware"
	"github.com/martinezsaweczko/claudeai-api/model"
	"github.com/martinezsaweczko/claudeai-api/o11"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// HealthHandlerConfig holds dependencies for the health handler
type HealthHandlerConfig struct {
	Log           *slog.Logger
	Observability *o11.ObservabilityInst
	Cfg           *config.Config
	BasePath      string
}

// HealthHandler handles health check requests
type HealthHandler struct {
	log          *slog.Logger
	cfg          *config.Config
	handlerCalls metric.Int64Counter
	basePath     string
}

// NewHealthHandler creates a new health handler
func (h *HealthHandlerConfig) NewHealthHandler() (*HealthHandler, error) {
	handlerCalls, err := h.Observability.MeterProvider.Meter("health_handler").Int64Counter("health_handler_calls", metric.WithDescription("Number of calls to the health handler"))
	if err != nil {
		h.Log.Error("Failed to create health handler counter", "error", err)
		return nil, err
	}

	return &HealthHandler{
		log:          h.Log,
		cfg:          h.Cfg,
		handlerCalls: handlerCalls,
		basePath:     h.BasePath,
	}, nil
}

// RegisterRoutes registers the health handler route
func (h *HealthHandler) RegisterRoutes(router Router, middlewares ...middleware.Middleware) {
	path := fmt.Sprintf("%s/health", h.basePath)
	if len(middlewares) > 0 {
		router.HandleFuncWithMiddleware("GET "+path, h.getHealth, middlewares...)
	} else {
		router.HandleFunc("GET "+path, h.getHealth)
	}
}

// Get health status
//
//	@Summary      Get health status
//	@Description  Returns the current health status of the application.
//	@Tags         System
//	@Accept       json
//	@Produce      json
//	@Success      200  {object}  model.HealthInfo
//	@Router       /api/v1/health [get]
func (h *HealthHandler) getHealth(w http.ResponseWriter, r *http.Request) {
	// Probed continuously by the orchestrator: no span and no Info log per request
	h.log.Debug("Received request for health status")

	healthInfo := &model.HealthInfo{
		Status:    "ok",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(healthInfo); err != nil {
		h.log.Error("Failed to encode health information", "error", err)
		h.handlerCalls.Add(r.Context(), 1,
			metric.WithAttributes(attribute.String("method", "GET")),
			metric.WithAttributes(attribute.Int("http_code", http.StatusInternalServerError)),
			metric.WithAttributes(attribute.String("path", fmt.Sprintf("%s/health", h.basePath))),
		)
		return
	}

	h.handlerCalls.Add(r.Context(), 1,
		metric.WithAttributes(attribute.String("method", "GET")),
		metric.WithAttributes(attribute.Int("http_code", http.StatusOK)),
		metric.WithAttributes(attribute.String("path", fmt.Sprintf("%s/health", h.basePath))),
	)
}
