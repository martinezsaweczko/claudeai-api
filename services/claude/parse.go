package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

const (
	maxLineBytes       = 16 << 20 // stream-json lines (assistant messages) can be large
	maxErrMessageRunes = 500
	syntheticModel     = "<synthetic>" // model name the CLI uses for locally generated errors
	turnSeparator      = "\n\n"        // between text of different assistant turns
)

type rawUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
}

// rawLine is the union of the fields we read from any stream-json line.
type rawLine struct {
	Type  string `json:"type"`
	Event *struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
	Message *struct {
		Model string `json:"model"`
	} `json:"message"`

	// result line
	IsError        bool     `json:"is_error"`
	Result         string   `json:"result"`
	StopReason     string   `json:"stop_reason"`
	APIErrorStatus int      `json:"api_error_status"`
	Usage          rawUsage `json:"usage"`
	TotalCostUSD   float64  `json:"total_cost_usd"`
	DurationMS     int64    `json:"duration_ms"`
}

// streamParser turns stream-json lines into Events and accumulates the final
// Result. It is not safe for concurrent use.
type streamParser struct {
	text      strings.Builder
	sawDelta  bool
	newTurn   bool // a message_start was seen since the last text delta
	model     string
	gotResult bool
	isError   bool
	status    int
	result    Result
	errText   string
}

func newStreamParser() *streamParser { return &streamParser{} }

// feed parses one line. Malformed JSON returns an error and leaves the parser
// state untouched; blank lines and unknown event types are ignored.
func (p *streamParser) feed(line []byte) ([]Event, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, nil
	}
	var l rawLine
	if err := json.Unmarshal(line, &l); err != nil {
		return nil, fmt.Errorf("malformed stream line: %w", err)
	}

	switch l.Type {
	case "stream_event":
		if l.Event != nil && l.Event.Type == "content_block_delta" && l.Event.Delta.Type == "text_delta" && l.Event.Delta.Text != "" {
			text := l.Event.Delta.Text
			// A new assistant turn (after tool use) that follows earlier text
			// is separated by a blank line so the turns do not run together.
			if p.newTurn && p.text.Len() > 0 {
				text = turnSeparator + text
			}
			p.newTurn = false
			p.sawDelta = true
			p.text.WriteString(text)
			return []Event{{Type: EventTextDelta, Text: text}}, nil
		}
		// Tool use (content_block_start/input_json_delta) and thinking blocks
		// carry no user-visible text and are ignored.
		if l.Event != nil && l.Event.Type == "message_start" {
			p.newTurn = true
		}
	case "assistant":
		if l.Message != nil && l.Message.Model != "" && l.Message.Model != syntheticModel {
			p.model = l.Message.Model
		}
	case "result":
		return p.onResult(&l), nil
	}
	return nil, nil
}

func (p *streamParser) onResult(l *rawLine) []Event {
	p.gotResult = true
	p.isError = l.IsError
	p.status = l.APIErrorStatus
	if l.IsError {
		p.errText = l.Result
		return nil
	}

	// Result.Text is the text of every assistant turn, exactly what a streaming
	// client received. The CLI's result.result only carries the last turn, so
	// it is used only when no partial messages were delivered.
	text := p.text.String()
	if !p.sawDelta {
		text = l.Result
	}
	p.result = Result{
		Text:  text,
		Model: p.model,
		Usage: Usage{
			InputTokens:         l.Usage.InputTokens,
			OutputTokens:        l.Usage.OutputTokens,
			CacheCreationTokens: l.Usage.CacheCreationTokens,
			CacheReadTokens:     l.Usage.CacheReadTokens,
		},
		CostUSD:    l.TotalCostUSD,
		DurationMS: l.DurationMS,
		StopReason: l.StopReason,
	}

	var events []Event
	if !p.sawDelta && text != "" {
		// Partial messages were not delivered; surface the final text as one delta.
		events = append(events, Event{Type: EventTextDelta, Text: text})
	}
	res := p.result
	return append(events, Event{Type: EventResult, Result: &res})
}

// hasResult reports whether a result line was seen.
func (p *streamParser) hasResult() bool { return p.gotResult }

// finish returns the final result, or the typed error when the CLI reported a
// failure or the stream ended without a result.
func (p *streamParser) finish() (Result, error) {
	if !p.gotResult {
		return Result{}, fmt.Errorf("%w: stream ended without a result", ErrCLI)
	}
	if p.isError {
		return Result{}, classifyResultError(p.status, p.errText)
	}
	return p.result, nil
}

var (
	authTextRe  = regexp.MustCompile(`(?i)invalid api key|run /login|not logged in|oauth token|authentication|unauthorized|invalid bearer`)
	limitTextRe = regexp.MustCompile(`(?i)usage limit|rate limit|limit reached|too many requests|quota`)
	secretRe    = regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]+`)
)

// classifyResultError maps an is_error result (HTTP-like status plus text) to a
// typed, sanitized error.
func classifyResultError(status int, text string) error {
	msg := sanitize(text)
	switch {
	case status == 401 || status == 403 || authTextRe.MatchString(text):
		return fmt.Errorf("%w: %s", ErrAuth, msg)
	case status == 429 || limitTextRe.MatchString(text):
		return fmt.Errorf("%w: %s", ErrRateLimited, msg)
	case status == 404 && strings.Contains(strings.ToLower(text), "model"):
		return fmt.Errorf("%w: %s", ErrInvalidModel, msg)
	}
	return fmt.Errorf("%w: %s", ErrCLI, msg)
}

// sanitize makes CLI-provided text safe for errors and logs: known secrets and
// token shapes are redacted, control characters and runs of whitespace are
// collapsed, and the result is truncated.
func sanitize(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "[redacted]")
		}
	}
	s = secretRe.ReplaceAllString(s, "[redacted]")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxErrMessageRunes {
		s = string(r[:maxErrMessageRunes]) + "…"
	}
	return s
}
