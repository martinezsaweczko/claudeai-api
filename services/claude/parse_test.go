package claude

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// feedFixture runs every line of a testdata file through a fresh parser.
func feedFixture(t *testing.T, name string) (*streamParser, []Event) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	p := newStreamParser()
	var events []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), maxLineBytes)
	for sc.Scan() {
		evs, err := p.feed(sc.Bytes())
		if err != nil {
			t.Fatalf("feed(%s): %v", name, err)
		}
		events = append(events, evs...)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return p, events
}

func TestParserSuccessFixture(t *testing.T) {
	p, events := feedFixture(t, "success.stream.jsonl")

	var deltas []string
	for _, e := range events {
		if e.Type == EventTextDelta {
			deltas = append(deltas, e.Text)
		}
	}
	if want := []string{"OK"}; !reflect.DeepEqual(deltas, want) {
		t.Fatalf("deltas = %q, want %q", deltas, want)
	}

	res, err := p.finish()
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if res.Text != "OK" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.Model != "claude-sonnet-5-5" {
		t.Errorf("Model = %q", res.Model)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("StopReason = %q", res.StopReason)
	}
	wantUsage := Usage{InputTokens: 2, OutputTokens: 4, CacheCreationTokens: 868}
	if res.Usage != wantUsage {
		t.Errorf("Usage = %+v, want %+v", res.Usage, wantUsage)
	}
	if res.CostUSD <= 0 || res.DurationMS <= 0 {
		t.Errorf("cost=%v duration=%v, want positive", res.CostUSD, res.DurationMS)
	}
}

func TestParserErrorFixtures(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    error
		msg     string
	}{
		{"invalid model", "invalid_model.stream.jsonl", ErrInvalidModel, "nonexistent-model-xyz"},
		{"auth", "auth_error.stream.jsonl", ErrAuth, "/login"},
		{"rate limited", "rate_limited.stream.jsonl", ErrRateLimited, "usage limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, events := feedFixture(t, tt.fixture)
			for _, e := range events {
				if e.Type == EventTextDelta {
					t.Fatalf("error stream must not emit text deltas, got %q", e.Text)
				}
			}
			_, err := p.finish()
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if !containsFold(err.Error(), tt.msg) {
				t.Errorf("err %q does not mention %q", err, tt.msg)
			}
		})
	}
}

func TestParserTruncatedStream(t *testing.T) {
	p, _ := feedFixture(t, "truncated.stream.jsonl")
	_, err := p.finish()
	if !errors.Is(err, ErrCLI) {
		t.Fatalf("err = %v, want ErrCLI", err)
	}
}

func TestParserMalformedAndUnknownLines(t *testing.T) {
	p := newStreamParser()

	if _, err := p.feed([]byte(`{"type": "stream_event", `)); err == nil {
		t.Fatal("malformed JSON must return an error")
	}
	// Noise and unknown event types are tolerated and produce nothing.
	for _, line := range []string{``, `   `, `{"type":"rate_limit_event"}`, `{"type":"future_thing","x":1}`, `{"type":"system","subtype":"hook_started"}`} {
		evs, err := p.feed([]byte(line))
		if err != nil || len(evs) != 0 {
			t.Errorf("feed(%q) = %v, %v; want nothing", line, evs, err)
		}
	}
	// A valid delta after garbage still works.
	evs, err := p.feed([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}}`))
	if err != nil || len(evs) != 1 || evs[0].Text != "hi" {
		t.Fatalf("delta after garbage = %v, %v", evs, err)
	}
}

func TestParserIgnoresNonTextDeltas(t *testing.T) {
	p := newStreamParser()
	evs, err := p.feed([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"secret"}}}`))
	if err != nil || len(evs) != 0 {
		t.Fatalf("thinking delta = %v, %v; want nothing", evs, err)
	}
}

func TestParserResultWithoutDeltasEmitsText(t *testing.T) {
	p := newStreamParser()
	evs, err := p.feed([]byte(`{"type":"result","subtype":"success","is_error":false,"result":"hello","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2},"total_cost_usd":0.5,"duration_ms":7}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Type != EventTextDelta || evs[0].Text != "hello" || evs[1].Type != EventResult {
		t.Fatalf("events = %+v", evs)
	}
	res, err := p.finish()
	if err != nil || res.Text != "hello" {
		t.Fatalf("finish = %+v, %v", res, err)
	}
}

func TestClassifyResultError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		text   string
		want   error
	}{
		{"401", 401, "whatever", ErrAuth},
		{"403", 403, "whatever", ErrAuth},
		{"login text", 0, "Please run /login", ErrAuth},
		{"invalid key text", 0, "Invalid API key", ErrAuth},
		{"429", 429, "x", ErrRateLimited},
		{"usage limit text", 0, "Claude usage limit reached", ErrRateLimited},
		{"404 model", 404, "There's an issue with the selected model (foo)", ErrInvalidModel},
		{"404 other", 404, "not found", ErrCLI},
		{"500", 500, "boom", ErrCLI},
		{"plain", 0, "boom", ErrCLI},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := classifyResultError(tt.status, tt.text); !errors.Is(err, tt.want) {
				t.Fatalf("classifyResultError = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestSanitize(t *testing.T) {
	long := ""
	for range 2000 {
		long += "x"
	}
	tests := []struct {
		name, in, want string
		secrets        []string
	}{
		{"whitespace", "a\n\n  b\tc", "a b c", nil},
		{"control chars", "a\x00\x1b[31mb", "a [31mb", nil},
		{"token regex", "key sk-ant-oat01-abcDEF_123-xyz leaked", "key [redacted] leaked", nil},
		{"explicit secret", "tok SECRETVALUE end", "tok [redacted] end", []string{"SECRETVALUE"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitize(tt.in, tt.secrets...); got != tt.want {
				t.Fatalf("sanitize = %q, want %q", got, tt.want)
			}
		})
	}
	if got := sanitize(long); len([]rune(got)) > maxErrMessageRunes+1 {
		t.Errorf("sanitize did not truncate: %d runes", len([]rune(got)))
	}
}
