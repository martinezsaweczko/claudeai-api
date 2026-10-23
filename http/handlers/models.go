package handlers

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/martinezsaweczko/claudeai-api/http/middleware"
)

const modelOwner = "anthropic"

// ModelsHandlerConfig holds dependencies for the models handler.
type ModelsHandlerConfig struct {
	Log      *slog.Logger
	Models   []string // model ids advertised to clients
	BasePath string   // e.g. "/v1"
}

// ModelsHandler serves the OpenAI-compatible model listing.
type ModelsHandler struct {
	log      *slog.Logger
	models   []modelObject
	basePath string
	created  time.Time
}

// modelObject is an OpenAI model entry.
type modelObject struct {
	ID      string `json:"id" example:"sonnet"`
	Object  string `json:"object" example:"model"`
	Created int64  `json:"created" example:"1700000000"`
	OwnedBy string `json:"owned_by" example:"anthropic"`
}

// modelList is the OpenAI list envelope.
type modelList struct {
	Object string        `json:"object" example:"list"`
	Data   []modelObject `json:"data"`
}

// NewModelsHandler creates the handler. The `created` value of every model is
// the process start time, because the CLI exposes no real release dates.
func (c *ModelsHandlerConfig) NewModelsHandler() *ModelsHandler {
	models := make([]modelObject, 0, len(c.Models))
	for _, id := range c.Models {
		models = append(models, modelObject{ID: id, Object: "model", OwnedBy: modelOwner})
	}
	return &ModelsHandler{log: c.Log, models: models, basePath: c.BasePath, created: time.Now()}
}

// RegisterRoutes registers GET {base}/models and GET {base}/models/{model}.
func (h *ModelsHandler) RegisterRoutes(router Router, middlewares ...middleware.Middleware) {
	register := func(pattern string, fn func(http.ResponseWriter, *http.Request)) {
		if len(middlewares) > 0 {
			router.HandleFuncWithMiddleware(pattern, fn, middlewares...)
		} else {
			router.HandleFunc(pattern, fn)
		}
	}
	register("GET "+h.basePath+"/models", h.listModels)
	register("GET "+h.basePath+"/models/{model}", h.getModel)
}

func (h *ModelsHandler) entry(m modelObject) modelObject {
	m.Created = h.created.Unix()
	return m
}

// List models
//
//	@Summary      List models
//	@Description  OpenAI-compatible model list. The ids come from the -claude-models flag; `created` is the process start time.
//	@Tags         OpenAI
//	@Produce      json
//	@Security     BearerAuth
//	@Success      200  {object}  modelList
//	@Failure      401  {object}  openAIErrorDoc
//	@Router       /v1/models [get]
func (h *ModelsHandler) listModels(w http.ResponseWriter, _ *http.Request) {
	list := modelList{Object: "list", Data: make([]modelObject, 0, len(h.models))}
	for _, m := range h.models {
		list.Data = append(list.Data, h.entry(m))
	}
	writeJSON(w, http.StatusOK, list)
}

// Get a model
//
//	@Summary      Retrieve a model
//	@Description  Returns one advertised model, or 404 `model_not_found`.
//	@Tags         OpenAI
//	@Produce      json
//	@Security     BearerAuth
//	@Param        model  path      string  true  "Model id"
//	@Success      200    {object}  modelObject
//	@Failure      401    {object}  openAIErrorDoc
//	@Failure      404    {object}  openAIErrorDoc
//	@Router       /v1/models/{model} [get]
func (h *ModelsHandler) getModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("model")
	for _, m := range h.models {
		if m.ID == id {
			writeJSON(w, http.StatusOK, h.entry(m))
			return
		}
	}
	middleware.WriteOpenAIError(w, http.StatusNotFound, "The model '"+id+"' does not exist.", middleware.ErrorTypeInvalidRequest, "model_not_found")
}
