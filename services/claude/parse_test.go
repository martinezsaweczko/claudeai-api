package claude

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func deltasOf(events []Event) []string {
	var out []string
	for _, e := range events {
		if e.Type == EventTextDelta {
			out = append(out, e.Text)
		}
	}
	return out
}

func TestParserSkillInvocationFixture(t *testing.T) {
	p, events := feedFixture(t, "skill_invocation.stream.jsonl")
	deltas := deltasOf(events)
	if len(deltas) == 0 {
		t.Fatal("no text deltas")
	}
	joined := strings.Join(deltas, "")
	for _, leak := range []string{"tool_use", "toolu_", "partial_json", "\"skill\"", "Launching skill", "Base directory", "Skill"} {
		if strings.Contains(joined, leak) {
			t.Errorf("tool content leaked into text (%q): %q", leak, joined)
		}
	}
	if strings.HasPrefix(joined, "\n") {
		t.Errorf("first turn emitted no text, so no separator is allowed: %q", joined)
	}
	res, err := p.finish()
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != joined {
		t.Errorf("Result.Text %q != concatenated deltas %q", res.Text, joined)
	}
	if res.Model != "claude-haiku-5-5" || res.StopReason != "end_turn" {
		t.Errorf("model/stop = %q/%q", res.Model, res.StopReason)
	}
	if res.Usage.OutputTokens == 0 || res.CostUSD <= 0 {
		t.Errorf("usage/cost not taken from the final result: %+v %v", res.Usage, res.CostUSD)
	}
}

func textDeltaLine(text string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": text}}})
	return b
}

func TestParserSeparatesTurnsOnlyAfterEarlierText(t *testing.T) {
	msgStart := []byte(`{"type":"stream_event","event":{"type":"message_start"}}`)
	toolDelta := []byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"skill\":\"x\"}"}}}`)
	result := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"LAST TURN ONLY","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2},"num_turns":3}`)

	p := newStreamParser()
	var got []string
	feed := func(line []byte) {
		evs, err := p.feed(line)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, deltasOf(evs)...)
	}
	feed(msgStart)
	feed(textDeltaLine("Let me check."))
	feed(textDeltaLine(" One moment."))
	feed(toolDelta) // never leaks
	feed(msgStart)  // second turn
	feed(textDeltaLine("Done."))
	feed(textDeltaLine(" Bye."))
	feed(msgStart) // third turn without text: nothing to separate
	feed(msgStart)
	feed(textDeltaLine("Final."))
	want := []string{"Let me check.", " One moment.", "\n\nDone.", " Bye.", "\n\nFinal."}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deltas = %q, want %q", got, want)
	}
	feed(result)
	res, err := p.finish()
	if err != nil {
		t.Fatal(err)
	}
	if wantText := strings.Join(want, ""); res.Text != wantText {
		t.Fatalf("Text = %q, want accumulated %q (not the last turn only)", res.Text, wantText)
	}
}

func TestParserWebSearchFixture(t *testing.T) {
	p, events := feedFixture(t, "web_search.stream.jsonl")
	joined := strings.Join(deltasOf(events), "")
	if !strings.HasPrefix(joined, "El último partido") {
		t.Fatalf("text = %q", joined)
	}
	for _, leak := range []string{"tool_use", "toolu_", "partial_json", "WebSearch", "Web search results", "Links:", "\"query\"", "server_tool_use"} {
		if strings.Contains(joined, leak) {
			t.Errorf("web tool content leaked into text (%q)", leak)
		}
	}
	res, err := p.finish()
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != joined {
		t.Errorf("Result.Text != concatenated deltas")
	}
	// The real result line reports usage.server_tool_use as 0 although two
	// searches ran; modelUsage.webSearchRequests (2) and the tool_use blocks are
	// what the parser must rely on.
	if res.WebSearchRequests != 2 || res.WebFetchRequests != 0 {
		t.Errorf("web requests = search %d fetch %d, want 2/0", res.WebSearchRequests, res.WebFetchRequests)
	}
	if res.NumTurns != 3 || res.CostUSD <= 0 || len(res.SkillsInvoked) != 0 {
		t.Errorf("result = %+v", res)
	}
}

func TestParserWebToolsAcrossTurnsNeverLeak(t *testing.T) {
	lines := []string{
		`{"type":"stream_event","event":{"type":"message_start"}}`,
		string(textDeltaLine("Let me look it up.")),
		`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"tool_use","name":"WebSearch"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"query\":\"secret query\"}"}}}`,
		`{"type":"assistant","message":{"model":"claude-haiku-5-5","content":[{"type":"tool_use","id":"toolu_1","name":"WebSearch","input":{"query":"secret query"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"Web search results SECRET"}]}}`,
		`{"type":"stream_event","event":{"type":"message_start"}}`,
		`{"type":"assistant","message":{"model":"claude-haiku-5-5","content":[{"type":"server_tool_use","name":"web_search","input":{"query":"x"}},{"type":"web_search_tool_result","content":[{"title":"SECRET"}]},{"type":"tool_use","id":"toolu_2","name":"WebFetch","input":{"url":"https://example.com","prompt":"p"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"fetched SECRET"}]}}`,
		`{"type":"stream_event","event":{"type":"message_start"}}`,
		string(textDeltaLine("The score was 2-1.")),
		`{"type":"result","subtype":"success","is_error":false,"result":"The score was 2-1.","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2,"server_tool_use":{"web_search_requests":1,"web_fetch_requests":0}},"num_turns":3}`,
	}
	p := newStreamParser()
	var deltas []string
	for _, l := range lines {
		evs, err := p.feed([]byte(l))
		if err != nil {
			t.Fatal(err)
		}
		deltas = append(deltas, deltasOf(evs)...)
	}
	want := []string{"Let me look it up.", "\n\nThe score was 2-1."}
	if !reflect.DeepEqual(deltas, want) {
		t.Fatalf("deltas = %q, want %q", deltas, want)
	}
	res, err := p.finish()
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != strings.Join(want, "") {
		t.Errorf("Text = %q", res.Text)
	}
	// usage reports 1 search; one WebSearch tool_use block was seen: max wins.
	if res.WebSearchRequests != 1 || res.WebFetchRequests != 1 {
		t.Errorf("web requests = search %d fetch %d, want 1/1", res.WebSearchRequests, res.WebFetchRequests)
	}
}

func TestParserWebCountsFromResultOnly(t *testing.T) {
	p := newStreamParser()
	_, err := p.feed([]byte(`{"type":"result","subtype":"success","is_error":false,"result":"ok","usage":{"server_tool_use":{"web_search_requests":3,"web_fetch_requests":2}},"modelUsage":{"a":{"webSearchRequests":2},"b":{"webSearchRequests":2}}}`))
	if err != nil {
		t.Fatal(err)
	}
	res, _ := p.finish()
	if res.WebSearchRequests != 4 || res.WebFetchRequests != 2 {
		t.Errorf("web requests = %d/%d, want 4 (sum of modelUsage beats usage 3) / 2", res.WebSearchRequests, res.WebFetchRequests)
	}
}
