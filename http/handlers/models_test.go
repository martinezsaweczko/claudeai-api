package handlers

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newModelsRouter(models []string) *muxRouter {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := (&ModelsHandlerConfig{Log: log, Models: models, BasePath: "/v1"}).NewModelsHandler()
	h.created = time.Unix(1700000000, 0)
	router := &muxRouter{mux: http.NewServeMux()}
	h.RegisterRoutes(router)
	return router
}

func TestListModels(t *testing.T) {
	rec := httptest.NewRecorder()
	newModelsRouter([]string{"sonnet", "opus"}).mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var got struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Object != "list" || len(got.Data) != 2 {
		t.Fatalf("list = %+v", got)
	}
	for i, id := range []string{"sonnet", "opus"} {
		d := got.Data[i]
		if d.ID != id || d.Object != "model" || d.Created != 1700000000 || d.OwnedBy != "anthropic" {
			t.Errorf("data[%d] = %+v", i, d)
		}
	}
}

func TestGetModel(t *testing.T) {
	router := newModelsRouter([]string{"sonnet"})

	rec := httptest.NewRecorder()
	router.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models/sonnet", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("known model status = %d", rec.Code)
	}
	var m struct {
		ID, Object string
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m.ID != "sonnet" || m.Object != "model" {
		t.Errorf("model = %+v", m)
	}

	rec = httptest.NewRecorder()
	router.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown model status = %d", rec.Code)
	}
	if e := decodeErr(t, rec); e.Error.Code != "model_not_found" || e.Error.Type != "invalid_request_error" {
		t.Errorf("error = %+v", e.Error)
	}
}
