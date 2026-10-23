package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/martinezsaweczko/whatsappBot-golang/http/middleware"
	"github.com/martinezsaweczko/whatsappBot-golang/model"
	"github.com/martinezsaweczko/whatsappBot-golang/services/scheduler"
)

// maxScheduleBodyBytes caps the size of scheduler request bodies.
const maxScheduleBodyBytes = 16 << 10

// SchedulerService is the interface consumed by the scheduler HTTP handler.
type SchedulerService interface {
	Create(ctx context.Context, cmd model.ScheduledCommand) (uuid.UUID, error)
	List(ctx context.Context) ([]model.ScheduledCommand, error)
	// Update replaces a scheduled command. It reports an unknown ID with an
	// error matching sql.ErrNoRows and a rejected command with one matching
	// scheduler.ErrInvalid.
	Update(ctx context.Context, cmd model.ScheduledCommand) (model.ScheduledCommand, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

// SchedulerHandlerConfig holds dependencies for the scheduler handler.
type SchedulerHandlerConfig struct {
	Log      *slog.Logger
	Service  SchedulerService
	BasePath string
}

// SchedulerHandler exposes the scheduled commands REST API.
type SchedulerHandler struct {
	log      *slog.Logger
	service  SchedulerService
	basePath string
}

// createScheduleRequest is the body of POST /scheduler.
type createScheduleRequest struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Command  string `json:"command"`
	GroupJID string `json:"group_jid"`
}

// updateScheduleRequest is the body of PUT /scheduler/{id}. It replaces every
// editable field; Enabled defaults to true when omitted.
type updateScheduleRequest struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Command  string `json:"command"`
	GroupJID string `json:"group_jid"`
	Enabled  *bool  `json:"enabled,omitempty"`
}

// scheduledCommandResponse is the JSON representation of a scheduled command.
type scheduledCommandResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Schedule  string `json:"schedule"`
	Command   string `json:"command"`
	GroupJID  string `json:"group_jid"`
	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// newScheduledCommandResponse renders a stored scheduled command.
func newScheduledCommandResponse(c model.ScheduledCommand) scheduledCommandResponse {
	return scheduledCommandResponse{
		ID:        c.ID.String(),
		Name:      c.Name,
		Schedule:  c.Schedule,
		Command:   c.Command,
		GroupJID:  c.GroupJID,
		Enabled:   c.Enabled,
		CreatedAt: c.CreatedAt.Format(time.RFC3339),
		UpdatedAt: c.UpdatedAt.Format(time.RFC3339),
	}
}

// NewSchedulerHandler creates a new scheduler handler.
func (c SchedulerHandlerConfig) NewSchedulerHandler() (*SchedulerHandler, error) {
	if c.Log == nil {
		return nil, fmt.Errorf("scheduler handler logger is required")
	}
	if c.Service == nil {
		return nil, fmt.Errorf("scheduler service is required")
	}
	return &SchedulerHandler{
		log:      c.Log,
		service:  c.Service,
		basePath: c.BasePath,
	}, nil
}

// RegisterRoutes registers POST and GET {basePath}/scheduler, and PUT and
// DELETE {basePath}/scheduler/{id}.
func (h *SchedulerHandler) RegisterRoutes(router Router, middlewares ...middleware.Middleware) {
	path := h.basePath + "/scheduler"
	idPath := path + "/{id}"

	register := func(method, pattern string, handler func(http.ResponseWriter, *http.Request)) {
		if len(middlewares) > 0 {
			router.HandleFuncWithMiddleware(method+" "+pattern, handler, middlewares...)
		} else {
			router.HandleFunc(method+" "+pattern, handler)
		}
	}

	register("POST", path, h.create)
	register("GET", path, h.list)
	register("PUT", idPath, h.update)
	register("DELETE", idPath, h.delete)
}

// Create a scheduled command.
//
//	@Summary      Create a scheduled command
//	@Description  Schedules a WhatsApp command to run automatically in a group using a cron expression.
//	@Tags         Scheduler
//	@Accept       json
//	@Produce      json
//	@Param        request body createScheduleRequest true "Scheduled command"
//	@Success      201 {object} scheduledCommandResponse
//	@Failure      400 {object} errorResponse
//	@Router       /api/v1/scheduler [post]
func (h *SchedulerHandler) create(w http.ResponseWriter, r *http.Request) {
	var req createScheduleRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxScheduleBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	req.Schedule = strings.TrimSpace(req.Schedule)
	req.Command = strings.TrimSpace(req.Command)
	req.GroupJID = strings.TrimSpace(req.GroupJID)

	cmd := model.ScheduledCommand{
		Name:     req.Name,
		Schedule: req.Schedule,
		Command:  req.Command,
		GroupJID: req.GroupJID,
		Enabled:  true,
	}

	id, err := h.service.Create(r.Context(), cmd)
	if err != nil {
		h.log.Error("Failed to create scheduled command", "error", err)
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, scheduledCommandResponse{
		ID:        id.String(),
		Name:      cmd.Name,
		Schedule:  cmd.Schedule,
		Command:   cmd.Command,
		GroupJID:  cmd.GroupJID,
		Enabled:   true,
		CreatedAt: "",
		UpdatedAt: "",
	})
}

// List scheduled commands.
//
//	@Summary      List scheduled commands
//	@Description  Returns all scheduled commands.
//	@Tags         Scheduler
//	@Produce      json
//	@Success      200 {array} scheduledCommandResponse
//	@Failure      502 {object} errorResponse
//	@Router       /api/v1/scheduler [get]
func (h *SchedulerHandler) list(w http.ResponseWriter, r *http.Request) {
	cmds, err := h.service.List(r.Context())
	if err != nil {
		h.log.Error("Failed to list scheduled commands", "error", err)
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: "failed to list scheduled commands"})
		return
	}

	resp := make([]scheduledCommandResponse, 0, len(cmds))
	for _, c := range cmds {
		resp = append(resp, newScheduledCommandResponse(c))
	}
	writeJSON(w, http.StatusOK, resp)
}

// Update a scheduled command.
//
//	@Summary      Update a scheduled command
//	@Description  Replaces the name, cron expression, command, group and enabled flag of a scheduled command, keeping its ID. The new schedule takes effect immediately. Omitting enabled sets it to true.
//	@Tags         Scheduler
//	@Accept       json
//	@Produce      json
//	@Param        id path string true "Schedule UUID"
//	@Param        request body updateScheduleRequest true "Scheduled command"
//	@Success      200 {object} scheduledCommandResponse
//	@Failure      400 {object} errorResponse
//	@Failure      404 {object} errorResponse
//	@Failure      500 {object} errorResponse
//	@Router       /api/v1/scheduler/{id} [put]
func (h *SchedulerHandler) update(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid schedule id"})
		return
	}

	var req updateScheduleRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxScheduleBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}

	cmd := model.ScheduledCommand{
		ID:       id,
		Name:     strings.TrimSpace(req.Name),
		Schedule: strings.TrimSpace(req.Schedule),
		Command:  strings.TrimSpace(req.Command),
		GroupJID: strings.TrimSpace(req.GroupJID),
		Enabled:  req.Enabled == nil || *req.Enabled,
	}

	updated, err := h.service.Update(r.Context(), cmd)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, newScheduledCommandResponse(updated))
	case errors.Is(err, scheduler.ErrInvalid):
		h.log.Warn("Rejected scheduled command update", "id", id.String(), "error", err)
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
	case errors.Is(err, sql.ErrNoRows):
		h.log.Warn("Scheduled command to update not found", "id", id.String())
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "scheduled command not found"})
	default:
		h.log.Error("Failed to update scheduled command", "id", id.String(), "error", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to update scheduled command"})
	}
}

// Delete a scheduled command.
//
//	@Summary      Delete a scheduled command
//	@Description  Removes a scheduled command by ID.
//	@Tags         Scheduler
//	@Param        id path string true "Schedule UUID"
//	@Success      204
//	@Failure      400 {object} errorResponse
//	@Failure      404 {object} errorResponse
//	@Failure      500 {object} errorResponse
//	@Router       /api/v1/scheduler/{id} [delete]
func (h *SchedulerHandler) delete(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimSpace(r.PathValue("id"))
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid schedule id"})
		return
	}

	if err := h.service.Delete(r.Context(), id); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		}
		h.log.Error("Failed to delete scheduled command", "id", id.String(), "error", err)
		writeJSON(w, status, errorResponse{Error: "failed to delete scheduled command"})
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
