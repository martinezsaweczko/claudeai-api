package handlers

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/martinezsaweczko/claudeai-api/http/middleware"
)

// SkillsHandlerConfig holds dependencies for the skills handler. A nil Source
// means skills are disabled.
type SkillsHandlerConfig struct {
	Log      *slog.Logger
	Source   SkillsSource
	Repo     string // configured repository (credentials are rejected by config validation)
	Ref      string
	BasePath string // e.g. "/api/v1"
}

// SkillsHandler serves the internal skills status and refresh endpoints. They
// are unauthenticated like health and metrics: the internal port must never be
// exposed publicly.
type SkillsHandler struct {
	log      *slog.Logger
	source   SkillsSource
	repo     string
	ref      string
	basePath string
}

// skillItem describes one available skill.
type skillItem struct {
	Name        string `json:"name" example:"summarize"`
	Plugin      string `json:"plugin,omitempty" example:"core-skills"`
	Description string `json:"description"`
}

// skippedItem describes a path in the repository that was ignored.
type skippedItem struct {
	Path   string `json:"path" example:"plugins/x/skills/bad"`
	Reason string `json:"reason"`
}

// skillsSyncStatus is the health of the periodic sync.
type skillsSyncStatus struct {
	LastAttempt         *time.Time `json:"last_attempt"`
	LastSuccess         *time.Time `json:"last_success"`
	LastError           string     `json:"last_error,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
}

// skillsResponse is the body of GET {base}/skills.
type skillsResponse struct {
	Enabled  bool              `json:"enabled"`
	Repo     string            `json:"repo,omitempty"`
	Ref      string            `json:"ref,omitempty"`
	Commit   string            `json:"commit,omitempty"`
	SyncedAt *time.Time        `json:"synced_at"`
	Skills   []skillItem       `json:"skills"`
	Skipped  []skippedItem     `json:"skipped"`
	Sync     *skillsSyncStatus `json:"sync,omitempty"`
}

// skillsDisabledResponse is the body of GET {base}/skills when skills are off.
type skillsDisabledResponse struct {
	Enabled bool `json:"enabled" example:"false"`
}

// skillsRefreshResponse is the body of a accepted refresh.
type skillsRefreshResponse struct {
	Status string `json:"status" example:"triggered"`
}

// NewSkillsHandler creates the handler.
func (c *SkillsHandlerConfig) NewSkillsHandler() *SkillsHandler {
	log := c.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &SkillsHandler{log: log, source: c.Source, repo: c.Repo, ref: c.Ref, basePath: c.BasePath}
}

// RegisterRoutes registers GET {base}/skills and POST {base}/skills/refresh.
// Other methods get 405 from the mux.
func (h *SkillsHandler) RegisterRoutes(router Router, middlewares ...middleware.Middleware) {
	register := func(pattern string, fn func(http.ResponseWriter, *http.Request)) {
		if len(middlewares) > 0 {
			router.HandleFuncWithMiddleware(pattern, fn, middlewares...)
		} else {
			router.HandleFunc(pattern, fn)
		}
	}
	register("GET "+h.basePath+"/skills", h.getSkills)
	register("POST "+h.basePath+"/skills/refresh", h.refresh)
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// Get skills
//
//	@Summary      Skills status
//	@Description  Lists the skills of the current snapshot and the health of the sync. Returns `{"enabled":false}` when no skills repository is configured. Internal port only: unauthenticated.
//	@Tags         Skills
//	@Produce      json
//	@Success      200  {object}  skillsResponse
//	@Router       /api/v1/skills [get]
func (h *SkillsHandler) getSkills(w http.ResponseWriter, _ *http.Request) {
	if h.source == nil {
		writeJSON(w, http.StatusOK, skillsDisabledResponse{Enabled: false})
		return
	}
	st := h.source.Status()
	resp := skillsResponse{
		Enabled: true,
		Repo:    h.repo,
		Ref:     h.ref,
		Skills:  []skillItem{},
		Skipped: []skippedItem{},
		Sync: &skillsSyncStatus{
			LastAttempt:         timePtr(st.LastAttempt),
			LastSuccess:         timePtr(st.LastSuccess),
			LastError:           st.LastError,
			ConsecutiveFailures: st.ConsecutiveFailures,
		},
	}
	if snap, ok := h.source.Current(); ok {
		resp.Commit = snap.Commit
		resp.SyncedAt = timePtr(snap.SyncedAt)
		for _, s := range snap.Skills {
			resp.Skills = append(resp.Skills, skillItem{Name: s.Name, Plugin: s.Plugin, Description: s.Description})
		}
		for _, s := range snap.Skipped {
			resp.Skipped = append(resp.Skipped, skippedItem{Path: s.Path, Reason: s.Reason})
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// Refresh skills
//
//	@Summary      Trigger a skills sync
//	@Description  Asks the background syncer to sync now (non-blocking, coalesced) and returns 202. Poll `GET /api/v1/skills` for the result. 404 `skills_disabled` when no skills repository is configured. Internal port only: unauthenticated.
//	@Tags         Skills
//	@Produce      json
//	@Success      202  {object}  skillsRefreshResponse
//	@Failure      404  {object}  errorResponse
//	@Router       /api/v1/skills/refresh [post]
func (h *SkillsHandler) refresh(w http.ResponseWriter, r *http.Request) {
	if h.source == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "skills_disabled"})
		return
	}
	h.source.Trigger()
	h.log.InfoContext(r.Context(), "Skills refresh requested")
	writeJSON(w, http.StatusAccepted, skillsRefreshResponse{Status: "triggered"})
}
