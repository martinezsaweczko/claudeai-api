package handlers

import (
	"context"
	"net/http"

	"github.com/martinezsaweczko/claudeai-api/http/middleware"
	"github.com/martinezsaweczko/claudeai-api/services/claude"
)

type Router interface {
	Handle(pattern string, handler http.Handler)
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
	HandleWithMiddleware(pattern string, handler http.Handler, middlewares ...middleware.Middleware)
	HandleFuncWithMiddleware(pattern string, handler func(http.ResponseWriter, *http.Request), middlewares ...middleware.Middleware)
}

// ChatBackend is the Claude capability consumed by the chat completions
// handler. It is satisfied by claude.Backend.
type ChatBackend interface {
	// Stream runs the request calling emit for every event; an error from emit
	// aborts the run (the CLI process is killed) and is returned.
	Stream(ctx context.Context, req claude.Request, emit func(claude.Event) error) (claude.Result, error)
	Complete(ctx context.Context, req claude.Request) (claude.Result, error)
}
