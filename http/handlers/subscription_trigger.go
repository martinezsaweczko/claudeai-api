package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/martinezsaweczko/whatsappBot-golang/http/middleware"
	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// SubscriptionNotifier sends subscription notifications (consumer-defined interface)
type SubscriptionNotifier interface {
	Notify(ctx context.Context, sender whatsapp.Sender, category, filename string) error
}

// SubscriptionTriggerHandlerConfig holds dependencies for the subscription trigger handler
type SubscriptionTriggerHandlerConfig struct {
	Log      *slog.Logger
	TP       trace.TracerProvider
	Notifier SubscriptionNotifier
	Sender   whatsapp.Sender
	BasePath string
}

// SubscriptionTriggerHandler exposes the internal endpoint that external systems
// call when a new file arrives, triggering subscription notifications
type SubscriptionTriggerHandler struct {
	log      *slog.Logger
	tracer   trace.Tracer
	notifier SubscriptionNotifier
	sender   whatsapp.Sender
	basePath string
}

// triggerRequest is the body of POST /file
type triggerRequest struct {
	Category string `json:"category"`
	File     string `json:"file"`
}

// statusResponse is the generic response of the trigger endpoint
type statusResponse struct {
	Status string `json:"status"`
}

// NewSubscriptionTriggerHandler creates a new subscription trigger handler
func (c *SubscriptionTriggerHandlerConfig) NewSubscriptionTriggerHandler() *SubscriptionTriggerHandler {
	return &SubscriptionTriggerHandler{
		log:      c.Log,
		tracer:   c.TP.Tracer("http/handlers/subscription_trigger"),
		notifier: c.Notifier,
		sender:   c.Sender,
		basePath: c.BasePath,
	}
}

// RegisterRoutes registers POST {basePath}/file
func (h *SubscriptionTriggerHandler) RegisterRoutes(router Router, middlewares ...middleware.Middleware) {
	path := h.basePath + "/file"
	if len(middlewares) > 0 {
		router.HandleFuncWithMiddleware("POST "+path, h.trigger, middlewares...)
	} else {
		router.HandleFunc("POST "+path, h.trigger)
	}
}

// Trigger subscription notifications.
//
//	@Summary      Trigger subscription notifications
//	@Description  Notifies all subscribers whose subscription text matches the provided filename.
//	@Tags         Quiosk
//	@Accept       json
//	@Produce      json
//	@Param        request body triggerRequest true "Subscription trigger"
//	@Success      200 {object} statusResponse
//	@Failure      400 {object} statusResponse
//	@Router       /api/v1/file [post]
func (h *SubscriptionTriggerHandler) trigger(w http.ResponseWriter, r *http.Request) {
	var req triggerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(statusResponse{Status: "Invalid body"})
		return
	}

	if req.Category == "" || req.File == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(statusResponse{Status: "category and file are required"})
		return
	}

	h.log.Info("New file trigger received", "category", req.Category, "file", req.File)

	// Fire-and-forget: notify subscribers in the background with a detached context
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		ctx, span := h.tracer.Start(ctx, "handler.SubscriptionTrigger", trace.WithAttributes(
			attribute.String("category", req.Category),
			attribute.String("file", req.File),
		))
		defer span.End()

		if err := h.notifier.Notify(ctx, h.sender, req.Category, req.File); err != nil {
			span.RecordError(err)
			h.log.Error("Failed to notify subscribers", "category", req.Category, "file", req.File, "error", err)
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(statusResponse{Status: "Sent"})
}
