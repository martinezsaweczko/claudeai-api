package claude

import (
	"context"
	"errors"
)

// Message roles accepted in a Request.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Message is one turn of the conversation.
type Message struct {
	Role    string
	Content string
}

// Request is a stateless completion request: the full history every time.
type Request struct {
	Model    string // alias or full model name; empty uses the configured default
	System   string
	Messages []Message // ends with a user message
}

// Usage reports token counts as the CLI does. InputTokens counts only uncached
// input; the total prompt size is InputTokens + CacheCreationTokens +
// CacheReadTokens.
type Usage struct {
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
}

// Result is the final outcome of a successful run.
type Result struct {
	Text       string
	Model      string // resolved model id reported by the CLI
	Usage      Usage
	CostUSD    float64
	DurationMS int64
	StopReason string
	NumTurns   int // model turns of the run (a Skill tool use adds one)

	// SkillsInvoked lists the skills the model used, in order (repeats kept).
	SkillsInvoked []string
}

// EventType identifies a streaming Event.
type EventType int

const (
	// EventTextDelta carries an incremental piece of the answer in Text.
	EventTextDelta EventType = iota + 1
	// EventResult is emitted last on success and carries the final Result.
	EventResult
)

// Event is a streaming notification.
type Event struct {
	Type   EventType
	Text   string  // EventTextDelta
	Result *Result // EventResult
}

// Backend is the Claude capability consumed by the HTTP layer. The CLI
// implementation can be replaced by an API-key backend later.
type Backend interface {
	// Stream runs the request and calls emit for every event in order. If emit
	// returns an error the run is aborted and that error is returned. emit may
	// be nil.
	Stream(ctx context.Context, req Request, emit func(Event) error) (Result, error)
	// Complete runs the request and returns the final result.
	Complete(ctx context.Context, req Request) (Result, error)
}

// Typed errors; use errors.Is. Wrapped errors carry a bounded, sanitized
// message safe to log (never the prompt or secrets).
var (
	ErrAuth           = errors.New("claude: authentication failed")
	ErrRateLimited    = errors.New("claude: usage limit reached")
	ErrTimeout        = errors.New("claude: timed out")
	ErrBusy           = errors.New("claude: backend busy")
	ErrCLI            = errors.New("claude: cli error")
	ErrNotFound       = errors.New("claude: cli binary not found")
	ErrInvalidModel   = errors.New("claude: model not found")
	ErrInvalidRequest = errors.New("claude: invalid request")
)
