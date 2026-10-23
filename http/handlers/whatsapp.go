package handlers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/martinezsaweczko/whatsappBot-golang/http/middleware"
	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
	"go.mau.fi/whatsmeow/types"
)

// GroupClient queries WhatsApp group information.
type GroupClient interface {
	GetJoinedGroups(ctx context.Context) ([]whatsapp.Group, error)
	GetGroupParticipants(ctx context.Context, groupJID types.JID) ([]whatsapp.GroupParticipant, error)
}

// WhatsAppHandlerConfig holds dependencies for the WhatsApp info handler.
type WhatsAppHandlerConfig struct {
	Log         *slog.Logger
	GroupClient GroupClient
	BasePath    string
}

// WhatsAppHandler exposes WhatsApp metadata endpoints.
type WhatsAppHandler struct {
	log         *slog.Logger
	groupClient GroupClient
	basePath    string
}

// groupResponse is the JSON representation of a WhatsApp group.
type groupResponse struct {
	JID  string `json:"jid"`
	Name string `json:"name"`
}

// participantResponse is the JSON representation of a group participant.
type participantResponse struct {
	JID          string `json:"jid"`
	PhoneNumber  string `json:"phone_number,omitempty"`
	DisplayName  string `json:"display_name,omitempty"`
	IsAdmin      bool   `json:"is_admin"`
	IsSuperAdmin bool   `json:"is_super_admin"`
}

// NewWhatsAppHandler creates a new WhatsApp info handler.
func (c WhatsAppHandlerConfig) NewWhatsAppHandler() (*WhatsAppHandler, error) {
	if c.Log == nil {
		return nil, fmt.Errorf("whatsapp handler logger is required")
	}
	if c.GroupClient == nil {
		return nil, fmt.Errorf("whatsapp group client is required")
	}
	return &WhatsAppHandler{
		log:         c.Log,
		groupClient: c.GroupClient,
		basePath:    c.BasePath,
	}, nil
}

// RegisterRoutes registers GET {basePath}/whatsapp/groups and
// GET {basePath}/whatsapp/groups/{jid}/participants.
func (h *WhatsAppHandler) RegisterRoutes(router Router, middlewares ...middleware.Middleware) {
	groupsPath := h.basePath + "/whatsapp/groups"
	participantsPath := groupsPath + "/{jid}/participants"

	register := func(pattern string, handler func(http.ResponseWriter, *http.Request)) {
		if len(middlewares) > 0 {
			router.HandleFuncWithMiddleware("GET "+pattern, handler, middlewares...)
		} else {
			router.HandleFunc("GET "+pattern, handler)
		}
	}

	register(groupsPath, h.listGroups)
	register(participantsPath, h.listParticipants)
}

// List joined WhatsApp groups.
//
//	@Summary      List WhatsApp groups
//	@Description  Returns the WhatsApp groups the bot is participating in.
//	@Tags         Whatsapp
//	@Produce      json
//	@Success      200 {array} groupResponse
//	@Failure      502 {object} errorResponse
//	@Router       /api/v1/whatsapp/groups [get]
func (h *WhatsAppHandler) listGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := h.groupClient.GetJoinedGroups(r.Context())
	if err != nil {
		h.log.Error("Failed to list WhatsApp groups", "error", err)
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: "failed to list groups"})
		return
	}

	resp := make([]groupResponse, 0, len(groups))
	for _, g := range groups {
		resp = append(resp, groupResponse{
			JID:  g.JID.String(),
			Name: g.Name,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// List participants of a WhatsApp group.
//
//	@Summary      List group participants
//	@Description  Returns the participants of the given WhatsApp group.
//	@Tags         Whatsapp
//	@Produce      json
//	@Param        jid path string true "Group JID"
//	@Success      200 {array} participantResponse
//	@Failure      400 {object} errorResponse
//	@Failure      502 {object} errorResponse
//	@Router       /api/v1/whatsapp/groups/{jid}/participants [get]
func (h *WhatsAppHandler) listParticipants(w http.ResponseWriter, r *http.Request) {
	jidStr := strings.TrimSpace(r.PathValue("jid"))
	if jidStr == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "group jid is required"})
		return
	}

	groupJID, err := types.ParseJID(jidStr)
	if err != nil || groupJID.User == "" || groupJID.Server == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid group jid"})
		return
	}

	participants, err := h.groupClient.GetGroupParticipants(r.Context(), groupJID)
	if err != nil {
		h.log.Error("Failed to list group participants", "jid", jidStr, "error", err)
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: "failed to list participants"})
		return
	}

	resp := make([]participantResponse, 0, len(participants))
	for _, p := range participants {
		resp = append(resp, participantResponse{
			JID:          p.JID.String(),
			PhoneNumber:  p.PhoneNumber.String(),
			DisplayName:  p.DisplayName,
			IsAdmin:      p.IsAdmin,
			IsSuperAdmin: p.IsSuperAdmin,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}
