package quote

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
	"go.mau.fi/whatsmeow/types"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

type fakeCompleter struct {
	calls      int
	lastPrompt string
	reply      string
	err        error
}

func (f *fakeCompleter) Complete(_ context.Context, prompt string) (string, error) {
	f.calls++
	f.lastPrompt = prompt
	return f.reply, f.err
}

// fakeSender records the text replies of the command handler
type fakeSender struct {
	replies []string
}

func (f *fakeSender) ReplyText(_ context.Context, _ whatsapp.IncomingMessage, text string) error {
	f.replies = append(f.replies, text)
	return nil
}

func (f *fakeSender) SendText(_ context.Context, _ types.JID, _ string) error { return nil }

func (f *fakeSender) ReplyMedia(_ context.Context, _ whatsapp.IncomingMessage, _ []byte, _, _ string) error {
	return nil
}

func (f *fakeSender) SendMedia(_ context.Context, _ types.JID, _ []byte, _, _ string) error {
	return nil
}

func (f *fakeSender) DeleteMessage(_ context.Context, _ whatsapp.IncomingMessage) error { return nil }

const quoteJSON = `[{"quote":"Always remember, success leaves clues.","author":"John Patrick Hickey","work":"","categories":["success"]}]`

// newServer serves body with the given status code
func newServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func newTestService(server *httptest.Server, ai Completer) *Service {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
	return New(server.Client(), server.URL, "test-key", ai, log, tracenoop.NewTracerProvider())
}

func TestRandomRequest(t *testing.T) {
	var gotMethod, gotKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotKey = r.Header.Get("X-Api-Key")
		w.Write([]byte(quoteJSON))
	}))
	defer server.Close()

	if _, err := newTestService(server, &fakeCompleter{reply: "Hola"}).Random(context.Background()); err != nil {
		t.Fatalf("Random failed: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotKey != "test-key" {
		t.Errorf("X-Api-Key = %q, want test-key", gotKey)
	}
}

func TestRandomMessage(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		reply string
		aiErr error
		want  string
	}{
		{
			name:  "with translation",
			body:  quoteJSON,
			reply: "Recuerda siempre: el éxito deja pistas.",
			want:  "*“Always remember, success leaves clues.”*\n_— John Patrick Hickey_\n\n🇪🇸 Recuerda siempre: el éxito deja pistas.",
		},
		{
			name:  "translation fails",
			body:  quoteJSON,
			aiErr: errors.New("openai down"),
			want:  "*“Always remember, success leaves clues.”*\n_— John Patrick Hickey_",
		},
		{
			name:  "translation empty after cleanup",
			body:  quoteJSON,
			reply: "  \n “ ” ",
			want:  "*“Always remember, success leaves clues.”*\n_— John Patrick Hickey_",
		},
		{
			name:  "cleanup of quote, author and translation",
			body:  `[{"quote":"  Be *bold*\n\nand   _brave_ \t ","author":" Jane\n _Doe_* ","work":"","categories":[]}]`,
			reply: "\"Sé *audaz*\n y   _valiente_\"",
			want:  "*“Be bold and brave”*\n_— Jane Doe_\n\n🇪🇸 Sé audaz y valiente",
		},
		{
			name:  "curly quotes stripped from translation",
			body:  quoteJSON,
			reply: "“Recuerda siempre.”",
			want:  "*“Always remember, success leaves clues.”*\n_— John Patrick Hickey_\n\n🇪🇸 Recuerda siempre.",
		},
		{
			name:  "empty author omits the author line",
			body:  `[{"quote":"Anonymous wisdom.","author":" * ","work":"","categories":[]}]`,
			reply: "Sabiduría anónima.",
			want:  "*“Anonymous wisdom.”*\n\n🇪🇸 Sabiduría anónima.",
		},
		{
			name:  "empty author without translation",
			body:  `[{"quote":"Anonymous wisdom.","categories":[]}]`,
			aiErr: errors.New("openai down"),
			want:  "*“Anonymous wisdom.”*",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newServer(t, http.StatusOK, tt.body)
			ai := &fakeCompleter{reply: tt.reply, err: tt.aiErr}

			got, err := newTestService(server, ai).Random(context.Background())
			if err != nil {
				t.Fatalf("Random failed: %v", err)
			}
			if got != tt.want {
				t.Errorf("message mismatch\ngot:  %q\nwant: %q", got, tt.want)
			}
		})
	}
}

func TestRandomTranslationPrompt(t *testing.T) {
	server := newServer(t, http.StatusOK, quoteJSON)
	ai := &fakeCompleter{reply: "Hola"}

	if _, err := newTestService(server, ai).Random(context.Background()); err != nil {
		t.Fatalf("Random failed: %v", err)
	}
	if ai.calls != 1 {
		t.Fatalf("AI calls = %d, want 1", ai.calls)
	}
	if !strings.Contains(ai.lastPrompt, "Always remember, success leaves clues.") {
		t.Errorf("prompt missing the quote: %s", ai.lastPrompt)
	}
	if !strings.Contains(strings.ToLower(ai.lastPrompt), "spanish") {
		t.Errorf("prompt does not ask for Spanish: %s", ai.lastPrompt)
	}
}

func TestRandomErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "non-200 status", status: http.StatusUnauthorized, body: `{"error":"bad key"}`},
		{name: "invalid JSON", status: http.StatusOK, body: `not json`},
		{name: "empty array", status: http.StatusOK, body: `[]`},
		{name: "empty quote", status: http.StatusOK, body: `[{"quote":"  *_ \n ","author":"Someone"}]`},
		{name: "null quote element", status: http.StatusOK, body: `[null]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newServer(t, tt.status, tt.body)
			ai := &fakeCompleter{reply: "Hola"}

			got, err := newTestService(server, ai).Random(context.Background())
			if err == nil {
				t.Fatalf("expected error, got message %q", got)
			}
			if ai.calls != 0 {
				t.Errorf("AI must not be called on quote failure, got %d calls", ai.calls)
			}
		})
	}
}

func TestRandomRequestFailure(t *testing.T) {
	server := newServer(t, http.StatusOK, quoteJSON)
	svc := newTestService(server, &fakeCompleter{})
	server.Close() // connection refused

	if _, err := svc.Random(context.Background()); err == nil {
		t.Fatal("expected error when the API is unreachable")
	}
}

func TestCommandPattern(t *testing.T) {
	svc := New(http.DefaultClient, "", "", &fakeCompleter{}, slog.Default(), tracenoop.NewTracerProvider())
	cmds := svc.Commands()
	if len(cmds) != 1 || cmds[0].Name != "quote" {
		t.Fatalf("unexpected commands: %+v", cmds)
	}
	pattern := cmds[0].Pattern

	tests := []struct {
		body string
		want bool
	}{
		{"quote", true},
		{"QUOTE", true},
		{"Quote", true},
		{"quote ", true},
		{"quotes", false},
		{"quote me", false},
		{"a quote", false},
		{"quotes about life", false},
	}
	for _, tt := range tests {
		if got := pattern.MatchString(tt.body); got != tt.want {
			t.Errorf("MatchString(%q) = %v, want %v", tt.body, got, tt.want)
		}
	}
}

func TestHandler(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantErr     bool
		wantReplies []string
	}{
		{
			name:        "success replies once with the message",
			status:      http.StatusOK,
			body:        quoteJSON,
			wantReplies: []string{"*“Always remember, success leaves clues.”*\n_— John Patrick Hickey_\n\n🇪🇸 Hola"},
		},
		{
			name:    "quote API failure sends nothing and returns an error",
			status:  http.StatusInternalServerError,
			body:    `oops`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newServer(t, tt.status, tt.body)
			svc := newTestService(server, &fakeCompleter{reply: "Hola"})
			sender := &fakeSender{}

			err := svc.Commands()[0].Handler(context.Background(), sender, whatsapp.IncomingMessage{Body: "quote", PushName: "David"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("handler error = %v, wantErr %v", err, tt.wantErr)
			}
			if len(sender.replies) != len(tt.wantReplies) {
				t.Fatalf("replies = %q, want %q", sender.replies, tt.wantReplies)
			}
			for i := range tt.wantReplies {
				if sender.replies[i] != tt.wantReplies[i] {
					t.Errorf("reply %d = %q, want %q", i, sender.replies[i], tt.wantReplies[i])
				}
			}
		})
	}
}
