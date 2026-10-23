package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/martinezsaweczko/whatsappBot-golang/http/middleware"
	"github.com/martinezsaweczko/whatsappBot-golang/model"
	"github.com/martinezsaweczko/whatsappBot-golang/services/scheduler"
)

// fakeSchedulerService is an in-memory scheduler service for tests.
type fakeSchedulerService struct {
	cmds []model.ScheduledCommand

	// updateErr and deleteErr, when set, are returned instead of acting.
	updateErr error
	deleteErr error
	// updated records the commands passed to Update.
	updated []model.ScheduledCommand
}

func (f *fakeSchedulerService) Create(ctx context.Context, cmd model.ScheduledCommand) (uuid.UUID, error) {
	if cmd.Schedule == "bad" {
		return uuid.Nil, errors.New("invalid schedule")
	}
	cmd.ID = uuid.Must(uuid.NewV7())
	f.cmds = append(f.cmds, cmd)
	return cmd.ID, nil
}

func (f *fakeSchedulerService) List(ctx context.Context) ([]model.ScheduledCommand, error) {
	return f.cmds, nil
}

// Update mirrors the real service: an unknown ID comes back as a wrapped
// sql.ErrNoRows, never the bare sentinel.
func (f *fakeSchedulerService) Update(ctx context.Context, cmd model.ScheduledCommand) (model.ScheduledCommand, error) {
	f.updated = append(f.updated, cmd)
	if f.updateErr != nil {
		return model.ScheduledCommand{}, f.updateErr
	}
	for i, c := range f.cmds {
		if c.ID == cmd.ID {
			cmd.CreatedAt = c.CreatedAt
			cmd.UpdatedAt = time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)
			f.cmds[i] = cmd
			return cmd, nil
		}
	}
	return model.ScheduledCommand{}, fmt.Errorf("failed to update scheduled command: %w", sql.ErrNoRows)
}

// Delete mirrors the real service: an unknown ID comes back as a wrapped
// sql.ErrNoRows, never the bare sentinel.
func (f *fakeSchedulerService) Delete(ctx context.Context, id uuid.UUID) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	for i, c := range f.cmds {
		if c.ID == id {
			f.cmds = append(f.cmds[:i], f.cmds[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("failed to delete scheduled command: %w", sql.ErrNoRows)
}

// serviceValidationError has the shape of the scheduler service's validation
// errors: it matches scheduler.ErrInvalid while its text is only the bare
// message, which is what the API returns to the client.
type serviceValidationError struct {
	msg string
}

func (e serviceValidationError) Error() string { return e.msg }

func (e serviceValidationError) Is(target error) bool { return target == scheduler.ErrInvalid }

func newSchedulerHandler(t *testing.T) (*SchedulerHandler, *fakeSchedulerService) {
	svc := &fakeSchedulerService{}
	h, err := SchedulerHandlerConfig{
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Service:  svc,
		BasePath: "/api/v1",
	}.NewSchedulerHandler()
	if err != nil {
		t.Fatalf("NewSchedulerHandler failed: %v", err)
	}
	return h, svc
}

func TestSchedulerCreate(t *testing.T) {
	h, svc := newSchedulerHandler(t)

	body, _ := json.Marshal(createScheduleRequest{
		Name:     "morning",
		Schedule: "0 8 * * *",
		Command:  "electricidad",
		GroupJID: "123456789@g.us",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/scheduler", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.create(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(svc.cmds) != 1 {
		t.Fatalf("expected 1 command, got %d", len(svc.cmds))
	}

	var resp scheduledCommandResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.ID == "" {
		t.Fatal("expected id in response")
	}
}

func TestSchedulerCreateValidation(t *testing.T) {
	h, _ := newSchedulerHandler(t)

	body, _ := json.Marshal(createScheduleRequest{
		Name:     "bad",
		Schedule: "bad",
		Command:  "x",
		GroupJID: "123@g.us",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/scheduler", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.create(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestSchedulerList(t *testing.T) {
	h, svc := newSchedulerHandler(t)
	svc.cmds = []model.ScheduledCommand{{ID: uuid.Must(uuid.NewV7()), Name: "morning", Schedule: "0 8 * * *", Command: "electricidad", GroupJID: "123@g.us"}}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/scheduler", nil)
	rec := httptest.NewRecorder()

	h.list(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp []scheduledCommandResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 response, got %d", len(resp))
	}
}

func TestSchedulerDelete(t *testing.T) {
	h, svc := newSchedulerHandler(t)
	id := uuid.Must(uuid.NewV7())
	svc.cmds = []model.ScheduledCommand{{ID: id, Name: "morning", Schedule: "0 8 * * *", Command: "electricidad", GroupJID: "123@g.us"}}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/scheduler/"+id.String(), nil)
	req.SetPathValue("id", id.String())
	rec := httptest.NewRecorder()

	h.delete(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
	if len(svc.cmds) != 0 {
		t.Fatalf("expected command to be deleted")
	}
}

func TestSchedulerDeleteNotFound(t *testing.T) {
	h, _ := newSchedulerHandler(t)

	id := uuid.Must(uuid.NewV7())
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/scheduler/"+id.String(), nil)
	req.SetPathValue("id", id.String())
	rec := httptest.NewRecorder()

	h.delete(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a wrapped sql.ErrNoRows, got %d", rec.Code)
	}
}

func TestSchedulerDeleteStoreFailure(t *testing.T) {
	h, svc := newSchedulerHandler(t)
	svc.deleteErr = errors.New("database is down")

	id := uuid.Must(uuid.NewV7())
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/scheduler/"+id.String(), nil)
	req.SetPathValue("id", id.String())
	rec := httptest.NewRecorder()

	h.delete(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

func TestSchedulerDeleteInvalidID(t *testing.T) {
	h, _ := newSchedulerHandler(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/scheduler/abc", nil)
	req.SetPathValue("id", "abc")
	rec := httptest.NewRecorder()

	h.delete(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestSchedulerUpdate(t *testing.T) {
	id := uuid.Must(uuid.NewV7())
	created := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	const validBody = `{"name":"evening","schedule":"30 20 * * *","command":"tiempo","group_jid":"987@g.us"}`

	tests := []struct {
		name        string
		pathID      string
		body        string
		updateErr   error
		wantStatus  int
		wantError   string
		wantEnabled bool
		wantCalled  bool
	}{
		{
			name:        "replaces the command and defaults enabled to true",
			pathID:      id.String(),
			body:        validBody,
			wantStatus:  http.StatusOK,
			wantEnabled: true,
			wantCalled:  true,
		},
		{
			name:        "explicit enabled false disables the command",
			pathID:      id.String(),
			body:        `{"name":"evening","schedule":"30 20 * * *","command":"tiempo","group_jid":"987@g.us","enabled":false}`,
			wantStatus:  http.StatusOK,
			wantEnabled: false,
			wantCalled:  true,
		},
		{
			name:        "trims surrounding whitespace",
			pathID:      id.String(),
			body:        `{"name":"  evening ","schedule":" 30 20 * * * ","command":" tiempo ","group_jid":" 987@g.us ","enabled":true}`,
			wantStatus:  http.StatusOK,
			wantEnabled: true,
			wantCalled:  true,
		},
		{
			name:       "invalid id",
			pathID:     "abc",
			body:       validBody,
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid schedule id",
		},
		{
			name:       "malformed body",
			pathID:     id.String(),
			body:       `{"name":`,
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid request body",
		},
		{
			name:       "unknown field",
			pathID:     id.String(),
			body:       `{"name":"evening","schedule":"30 20 * * *","command":"tiempo","group_jid":"987@g.us","id":"x"}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid request body",
		},
		{
			name:       "oversized body",
			pathID:     id.String(),
			body:       `{"name":"` + strings.Repeat("a", 17<<10) + `"}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid request body",
		},
		{
			name:       "validation failure reports the service message",
			pathID:     id.String(),
			body:       validBody,
			updateErr:  serviceValidationError{msg: "schedule is required"},
			wantStatus: http.StatusBadRequest,
			wantError:  "schedule is required",
			wantCalled: true,
		},
		{
			name:       "unknown id as a wrapped sql.ErrNoRows",
			pathID:     uuid.Must(uuid.NewV7()).String(),
			body:       validBody,
			wantStatus: http.StatusNotFound,
			wantError:  "scheduled command not found",
			wantCalled: true,
		},
		{
			name:       "store failure hides the internal error",
			pathID:     id.String(),
			body:       validBody,
			updateErr:  errors.New("database is down"),
			wantStatus: http.StatusInternalServerError,
			wantError:  "failed to update scheduled command",
			wantCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, svc := newSchedulerHandler(t)
			svc.cmds = []model.ScheduledCommand{{
				ID: id, Name: "morning", Schedule: "0 8 * * *", Command: "electricidad", GroupJID: "123@g.us",
				Enabled: true, CreatedAt: created, UpdatedAt: created,
			}}
			svc.updateErr = tt.updateErr

			req := httptest.NewRequest(http.MethodPut, "/api/v1/scheduler/"+tt.pathID, strings.NewReader(tt.body))
			req.SetPathValue("id", tt.pathID)
			rec := httptest.NewRecorder()

			h.update(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("expected %d, got %d: %s", tt.wantStatus, rec.Code, rec.Body.String())
			}
			if called := len(svc.updated) > 0; called != tt.wantCalled {
				t.Fatalf("service called = %v, want %v", called, tt.wantCalled)
			}

			if tt.wantStatus != http.StatusOK {
				var resp errorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("failed to decode error response: %v", err)
				}
				if resp.Error != tt.wantError {
					t.Fatalf("expected error %q, got %q", tt.wantError, resp.Error)
				}
				return
			}

			var resp scheduledCommandResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to decode response: %v", err)
			}
			want := scheduledCommandResponse{
				ID:        id.String(),
				Name:      "evening",
				Schedule:  "30 20 * * *",
				Command:   "tiempo",
				GroupJID:  "987@g.us",
				Enabled:   tt.wantEnabled,
				CreatedAt: "2026-01-01T08:00:00Z",
				UpdatedAt: "2026-10-12T09:30:00Z",
			}
			if resp != want {
				t.Fatalf("unexpected response:\n got %+v\nwant %+v", resp, want)
			}
			if got := svc.updated[0]; got.ID != id || got.Name != "evening" || got.Enabled != tt.wantEnabled {
				t.Fatalf("unexpected command passed to the service: %+v", got)
			}
		})
	}
}

// muxRouter adapts http.ServeMux to the Router interface so route patterns
// are exercised by the real Go 1.22 method-and-wildcard matching.
type muxRouter struct {
	mux             *http.ServeMux
	withMiddlewares int
}

func (m *muxRouter) Handle(pattern string, handler http.Handler) { m.mux.Handle(pattern, handler) }

func (m *muxRouter) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.mux.HandleFunc(pattern, handler)
}

func (m *muxRouter) HandleWithMiddleware(pattern string, handler http.Handler, _ ...middleware.Middleware) {
	m.withMiddlewares++
	m.mux.Handle(pattern, handler)
}

func (m *muxRouter) HandleFuncWithMiddleware(pattern string, handler func(http.ResponseWriter, *http.Request), _ ...middleware.Middleware) {
	m.withMiddlewares++
	m.mux.HandleFunc(pattern, handler)
}

func TestSchedulerRegisterRoutes(t *testing.T) {
	id := uuid.Must(uuid.NewV7())
	const body = `{"name":"evening","schedule":"30 20 * * *","command":"tiempo","group_jid":"987@g.us"}`

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{"create", http.MethodPost, "/api/v1/scheduler", body, http.StatusCreated},
		{"list", http.MethodGet, "/api/v1/scheduler", "", http.StatusOK},
		{"update reads the id from the path", http.MethodPut, "/api/v1/scheduler/" + id.String(), body, http.StatusOK},
		{"delete reads the id from the path", http.MethodDelete, "/api/v1/scheduler/" + id.String(), "", http.StatusNoContent},
		{"update without an id is not routed", http.MethodPut, "/api/v1/scheduler", body, http.StatusMethodNotAllowed},
		{"patch is not supported", http.MethodPatch, "/api/v1/scheduler/" + id.String(), body, http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, svc := newSchedulerHandler(t)
			svc.cmds = []model.ScheduledCommand{{ID: id, Name: "morning", Schedule: "0 8 * * *", Command: "electricidad", GroupJID: "123@g.us", Enabled: true}}
			router := &muxRouter{mux: http.NewServeMux()}
			h.RegisterRoutes(router)

			rec := httptest.NewRecorder()
			router.mux.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body)))

			if rec.Code != tt.wantStatus {
				t.Fatalf("expected %d, got %d: %s", tt.wantStatus, rec.Code, rec.Body.String())
			}
		})
	}

	t.Run("middlewares are applied to every route", func(t *testing.T) {
		h, _ := newSchedulerHandler(t)
		router := &muxRouter{mux: http.NewServeMux()}
		passthrough := func(next http.Handler) http.Handler { return next }
		h.RegisterRoutes(router, passthrough)

		if router.withMiddlewares != 4 {
			t.Fatalf("expected 4 routes registered with middlewares, got %d", router.withMiddlewares)
		}
	})
}
