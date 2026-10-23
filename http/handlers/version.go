package handlers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/martinezsaweczko/claudeai-api/config"
	"github.com/martinezsaweczko/claudeai-api/http/middleware"
	"github.com/martinezsaweczko/claudeai-api/model"
	"github.com/martinezsaweczko/claudeai-api/o11"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Router defines the interface for registering routes

type VersionHandlerConfig struct {
	Log           *slog.Logger
	Observability *o11.ObservabilityInst
	Cfg           *config.Config
	AppBuildInfo  model.BuildInfo
	BasePath      string
}

type VersionHandler struct {
	log           *slog.Logger
	observability *o11.ObservabilityInst
	cfg           *config.Config
	handlerCalls  metric.Int64Counter
	appBuildInfo  model.BuildInfo
	basePath      string
}

// NewVersionHandler creates a new version handler
func (v *VersionHandlerConfig) NewVersionHandler() (*VersionHandler, error) {
	// This is a wrapper to simplify imports in the app package

	handlerCalls, err := v.Observability.MeterProvider.Meter("version_handler").Int64Counter("version_handler_calls", metric.WithDescription("Number of calls to the version handler"))
	if err != nil {
		v.Log.Error("Failed to create version handler counter", "error", err)
		return nil, err

	}

	versionHandler := &VersionHandler{
		log:           v.Log,
		observability: v.Observability,
		cfg:           v.Cfg,
		handlerCalls:  handlerCalls,
		appBuildInfo:  v.AppBuildInfo,
		basePath:      v.BasePath,
	}
	return versionHandler, nil
}

// Register the routes for the version handler
func (v *VersionHandler) RegisterRoutes(router Router, middlewares ...middleware.Middleware) {
	// Register the version handler route
	path := fmt.Sprintf("%s/version", v.basePath)
	if len(middlewares) > 0 {
		router.HandleFuncWithMiddleware("GET "+path, v.getVersion, middlewares...)
	} else {
		router.HandleFunc("GET "+path, v.getVersion)
	}
}

// Get version information
//
//	@Summary      Get version information
//	@Description  Returns build and version metadata.
//	@Tags         System
//	@Accept       json
//	@Produce      json
//	@Success      200  {object}  model.VersionInfo
//	@Router       /api/v1/version [get]
func (v *VersionHandler) getVersion(w http.ResponseWriter, r *http.Request) {

	v.log.Info("Received request for version information")

	_, span := v.observability.Trace.Tracer("http/handlers/version").Start(r.Context(), "handler.GetVersion")
	defer span.End()

	versionInfo := &model.VersionInfo{
		Version:   v.appBuildInfo.Version,
		BuildTime: v.appBuildInfo.BuildTime,
		AppName:   v.appBuildInfo.AppName,
	}
	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(versionInfo)
	if err != nil {
		v.log.Error("Failed to encode version information", "error", err)
		v.handlerCalls.Add(r.Context(), 1, metric.WithAttributes(
			attribute.String("method", "GET")),
			metric.WithAttributes(attribute.Int("http_code", http.StatusInternalServerError)),
			metric.WithAttributes(attribute.String("path", fmt.Sprintf("%s/version", v.basePath))),
		)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
	v.handlerCalls.Add(r.Context(), 1, metric.WithAttributes(
		attribute.String("method", "GET")),
		metric.WithAttributes(attribute.Int("http_code", http.StatusOK)),
		metric.WithAttributes(attribute.String("path", fmt.Sprintf("%s/version", v.basePath))),
	)

}
