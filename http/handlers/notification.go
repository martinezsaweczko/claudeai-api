package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"unicode"

	"github.com/martinezsaweczko/whatsappBot-golang/http/middleware"
	"go.mau.fi/whatsmeow/types"
)

// NotificationSender sends a plain text message to a WhatsApp JID.
type NotificationSender interface {
	SendText(ctx context.Context, to types.JID, text string) error
}

// NotificationHandlerConfig holds dependencies for the notification handler.
type NotificationHandlerConfig struct {
	Log      *slog.Logger
	Sender   NotificationSender
	BasePath string
}

// NotificationHandler exposes an HTTP endpoint for sending WhatsApp text messages.
type NotificationHandler struct {
	log      *slog.Logger
	sender   NotificationSender
	basePath string
}

// notificationRequest is the body of POST {basePath}/notifications.
type notificationRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
}

// notificationResponse is returned when a notification is accepted.
type notificationResponse struct {
	Status string `json:"status"`
}

// errorResponse is a generic JSON error body.
type errorResponse struct {
	Error string `json:"error"`
}

// NewNotificationHandler creates a new notification handler.
func (c NotificationHandlerConfig) NewNotificationHandler() (*NotificationHandler, error) {
	if c.Log == nil {
		return nil, fmt.Errorf("notification handler logger is required")
	}
	if c.Sender == nil {
		return nil, fmt.Errorf("notification sender is required")
	}
	return &NotificationHandler{
		log:      c.Log,
		sender:   c.Sender,
		basePath: c.BasePath,
	}, nil
}

// RegisterRoutes registers POST {basePath}/notifications.
func (h *NotificationHandler) RegisterRoutes(router Router, middlewares ...middleware.Middleware) {
	path := h.basePath + "/notifications"
	if len(middlewares) > 0 {
		router.HandleFuncWithMiddleware("POST "+path, h.send, middlewares...)
	} else {
		router.HandleFunc("POST "+path, h.send)
	}
}

// Send a WhatsApp text notification.
//
//	@Summary      Send a WhatsApp notification
//	@Description  Sends a plain text message to the given WhatsApp recipient.
//	@Tags         Whatsapp
//	@Accept       json
//	@Produce      json
//	@Param        request body notificationRequest true "Notification"
//	@Success      202 {object} notificationResponse
//	@Failure      400 {object} errorResponse
//	@Failure      502 {object} errorResponse
//	@Router       /api/v1/notifications [post]
func (h *NotificationHandler) send(w http.ResponseWriter, r *http.Request) {
	var req notificationRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}

	req.Recipient = strings.TrimSpace(req.Recipient)
	req.Message = strings.TrimSpace(req.Message)
	if req.Recipient == "" || req.Message == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "recipient and message are required"})
		return
	}

	jid, err := parseRecipient(req.Recipient)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}

	h.log.Info("Sending WhatsApp notification", "recipient", req.Recipient, "jid", jid.String())

	if err := h.sender.SendText(r.Context(), jid, req.Message); err != nil {
		h.log.Error("Failed to send WhatsApp notification", "recipient", req.Recipient, "error", err)
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: "failed to send notification"})
		return
	}

	writeJSON(w, http.StatusAccepted, notificationResponse{Status: "sent"})
}

// parseRecipient converts a recipient string into a WhatsApp JID.
// If the string contains an "@" it is parsed as a full JID; legacy @c.us user
// JIDs are normalized to @s.whatsapp.net. Otherwise the string is treated as an
// E.164 phone number and converted to user@s.whatsapp.net.
func parseRecipient(recipient string) (types.JID, error) {
	if strings.Contains(recipient, "@") {
		jid, err := types.ParseJID(recipient)
		if err != nil || jid.User == "" || jid.Server == "" {
			return types.EmptyJID, fmt.Errorf("invalid WhatsApp JID: %s", recipient)
		}
		if jid.Server == "c.us" {
			jid = types.NewJID(jid.User, types.DefaultUserServer)
		}
		return jid, nil
	}

	phone := strings.TrimPrefix(strings.TrimSpace(recipient), "+")
	if err := validatePhone(phone); err != nil {
		return types.EmptyJID, err
	}
	return types.NewJID(phone, types.DefaultUserServer), nil
}

func validatePhone(phone string) error {
	if len(phone) < 8 || len(phone) > 15 {
		return fmt.Errorf("phone number must contain 8 to 15 digits")
	}
	for _, char := range phone {
		if !unicode.IsDigit(char) {
			return fmt.Errorf("phone number must contain only digits with an optional leading +")
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
