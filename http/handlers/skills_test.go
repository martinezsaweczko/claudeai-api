package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/martinezsaweczko/claudeai-api/services/skills"
)

type fakeSkills struct {
	snap     skills.Snapshot
	have     bool
	status   skills.Status
	triggers int
}

func (f *fakeSkills) Current() (skills.Snapshot, bool) { return f.snap, f.have }
func (f *fakeSkills) Status() skills.Status            { return f.status }
func (f *fakeSkills) Trigger()                         { f.triggers++ }

func skillsRouter(src SkillsSource) *muxRouter {
	r := &muxRouter{mux: http.NewServeMux()}
	(&SkillsHandlerConfig{Source: src, Repo: "git@github.com:o/r.git", Ref: "main", BasePath: "/api/v1"}).NewSkillsHandler().RegisterRoutes(r)
	return r
}

func serve(r *muxRouter, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestSkillsDisabled(t *testing.T) {
	r := skillsRouter(nil)
	rec := serve(r, http.MethodGet, "/api/v1/skills")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"enabled":false}` {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body.String())
	}
	rec = serve(r, http.MethodPost, "/api/v1/skills/refresh")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "skills_disabled") {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body.String())
	}
}

func TestSkillsGetBeforeFirstSync(t *testing.T) {
	f := &fakeSkills{status: skills.Status{LastAttempt: time.Unix(100, 0).UTC(), LastError: "boom", ConsecutiveFailures: 2}}
	rec := serve(skillsRouter(f), http.MethodGet, "/api/v1/skills")
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s %v", rec.Code, rec.Body.String(), err)
	}
	if got["enabled"] != true || got["synced_at"] != nil || got["commit"] != nil {
		t.Errorf("body = %v", got)
	}
	if s, _ := got["skills"].([]any); s == nil || len(s) != 0 {
		t.Errorf("skills must be an empty array, got %v", got["skills"])
	}
	sync := got["sync"].(map[string]any)
	if sync["last_success"] != nil || sync["last_error"] != "boom" || sync["consecutive_failures"] != float64(2) {
		t.Errorf("sync = %v", sync)
	}
}

func TestSkillsGetWithSnapshot(t *testing.T) {
	synced := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	f := &fakeSkills{
		have: true,
		snap: skills.Snapshot{
			Commit: "41e630a", Ref: "main", Dir: "/secret/state/dir", SyncedAt: synced,
			Skills:  []skills.SkillInfo{{Name: "summarize", Plugin: "core-skills", Description: "Summarize text"}},
			Skipped: []skills.Skipped{{Path: "plugins/x/skills/bad", Reason: "name mismatch"}},
		},
		status: skills.Status{LastAttempt: synced, LastSuccess: synced},
	}
	rec := serve(skillsRouter(f), http.MethodGet, "/api/v1/skills")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	body := rec.Body.String()
	if strings.Contains(body, "/secret/state/dir") {
		t.Errorf("snapshot dir leaked: %s", body)
	}
	var got skillsResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Repo != "git@github.com:o/r.git" || got.Ref != "main" || got.Commit != "41e630a" {
		t.Errorf("got = %+v", got)
	}
	if got.SyncedAt == nil || !got.SyncedAt.Equal(synced) {
		t.Errorf("synced_at = %v", got.SyncedAt)
	}
	if len(got.Skills) != 1 || got.Skills[0] != (skillItem{Name: "summarize", Plugin: "core-skills", Description: "Summarize text"}) {
		t.Errorf("skills = %+v", got.Skills)
	}
	if len(got.Skipped) != 1 || got.Skipped[0].Reason != "name mismatch" {
		t.Errorf("skipped = %+v", got.Skipped)
	}
	if got.Sync == nil || got.Sync.LastSuccess == nil || got.Sync.ConsecutiveFailures != 0 {
		t.Errorf("sync = %+v", got.Sync)
	}
}

func TestSkillsRefresh(t *testing.T) {
	f := &fakeSkills{}
	r := skillsRouter(f)
	rec := serve(r, http.MethodPost, "/api/v1/skills/refresh")
	if rec.Code != http.StatusAccepted || strings.TrimSpace(rec.Body.String()) != `{"status":"triggered"}` {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body.String())
	}
	if f.triggers != 1 {
		t.Fatalf("triggers = %d, want 1", f.triggers)
	}
}

func TestSkillsMethodsRestricted(t *testing.T) {
	f := &fakeSkills{}
	r := skillsRouter(f)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/skills/refresh"},
		{http.MethodPut, "/api/v1/skills/refresh"},
		{http.MethodPost, "/api/v1/skills"},
		{http.MethodDelete, "/api/v1/skills"},
	} {
		if rec := serve(r, tc.method, tc.path); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", tc.method, tc.path, rec.Code)
		}
	}
	if f.triggers != 0 {
		t.Errorf("triggers = %d, want 0", f.triggers)
	}
}
