package ai

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
	"github.com/sashabaranov/go-openai"
	"go.mau.fi/whatsmeow/types"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

type fakeTTS struct {
	lastText string
	audio    []byte
}

func (f *fakeTTS) Synthesize(_ context.Context, text string) ([]byte, error) {
	f.lastText = text
	return f.audio, nil
}

type fakeSender struct {
	replies  []string
	mediaOut [][]byte
}

func (f *fakeSender) ReplyText(_ context.Context, _ whatsapp.IncomingMessage, text string) error {
	f.replies = append(f.replies, text)
	return nil
}
func (f *fakeSender) SendText(_ context.Context, _ types.JID, _ string) error { return nil }
func (f *fakeSender) ReplyMedia(_ context.Context, _ whatsapp.IncomingMessage, data []byte, _, _ string) error {
	f.mediaOut = append(f.mediaOut, data)
	return nil
}
func (f *fakeSender) SendMedia(_ context.Context, _ types.JID, _ []byte, _, _ string) error {
	return nil
}
func (f *fakeSender) DeleteMessage(_ context.Context, _ whatsapp.IncomingMessage) error { return nil }

// fakeOpenAI serves classification + completion responses in order
func fakeOpenAI(t *testing.T, prompts *[]string, responses []string) *httptest.Server {
	t.Helper()
	i := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 {
			*prompts = append(*prompts, req.Messages[0].Content)
		}

		content := responses[i%len(responses)]
		i++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"message": map[string]string{"role": "assistant", "content": content}},
			},
		})
	}))
}

func newTestService(t *testing.T, server *httptest.Server, trolls []string, ttsSvc TTS) *Service {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc, err := New("test-key", "test-model", server.Client(), trolls, ttsSvc, log,
		tracenoop.NewTracerProvider(), noop.NewMeterProvider())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	// Point the OpenAI client at the test server
	cfg := openai.DefaultConfig("test-key")
	cfg.BaseURL = server.URL + "/v1"
	cfg.HTTPClient = server.Client()
	svc.client = openai.NewClientWithConfig(cfg)
	return svc
}

func TestFallbackTextReply(t *testing.T) {
	var prompts []string
	server := fakeOpenAI(t, &prompts, []string{"texto", "respuesta de prueba"})
	defer server.Close()

	svc := newTestService(t, server, nil, &fakeTTS{audio: []byte("mp3")})
	sender := &fakeSender{}

	msg := whatsapp.IncomingMessage{
		Body:   "@bot cuéntame algo",
		Sender: types.NewJID("34645568517", types.DefaultUserServer),
		Chat:   types.NewJID("34645568517", types.DefaultUserServer),
	}

	handler := svc.Fallback()
	if err := handler(context.Background(), sender, msg); err != nil {
		t.Fatalf("handler failed: %v", err)
	}

	if len(sender.replies) != 1 || sender.replies[0] != "respuesta de prueba" {
		t.Errorf("unexpected replies: %+v", sender.replies)
	}
	// First prompt is the audio/text classification
	if !strings.Contains(prompts[0], "audio o texto") {
		t.Errorf("missing classification prompt: %s", prompts[0])
	}
}

func TestFallbackTrollPrompt(t *testing.T) {
	var prompts []string
	server := fakeOpenAI(t, &prompts, []string{"texto", "respuesta troll"})
	defer server.Close()

	trollJID := types.NewJID("34645568517", types.DefaultUserServer).String()
	svc := newTestService(t, server, []string{trollJID}, &fakeTTS{audio: []byte("mp3")})
	sender := &fakeSender{}

	msg := whatsapp.IncomingMessage{
		Body:   "@bot pregunta seria",
		Sender: types.NewJID("34645568517", types.DefaultUserServer),
		Chat:   types.NewJID("34645568517", types.DefaultUserServer),
	}

	handler := svc.Fallback()
	if err := handler(context.Background(), sender, msg); err != nil {
		t.Fatalf("handler failed: %v", err)
	}

	// Second prompt (the actual completion) must contain the troll prefix
	if len(prompts) < 2 || !strings.Contains(prompts[1], "Eres un bufon") {
		t.Errorf("troll prompt not applied: %+v", prompts)
	}
}

func TestFallbackAudioReply(t *testing.T) {
	var prompts []string
	server := fakeOpenAI(t, &prompts, []string{"audio", "respuesta hablada"})
	defer server.Close()

	ttsSvc := &fakeTTS{audio: []byte("fake-mp3")}
	svc := newTestService(t, server, nil, ttsSvc)
	sender := &fakeSender{}

	msg := whatsapp.IncomingMessage{
		Body:   "@bot respóndeme con un audio",
		Sender: types.NewJID("34645568517", types.DefaultUserServer),
		Chat:   types.NewJID("34645568517", types.DefaultUserServer),
	}

	handler := svc.Fallback()
	if err := handler(context.Background(), sender, msg); err != nil {
		t.Fatalf("handler failed: %v", err)
	}

	if len(sender.mediaOut) != 1 || string(sender.mediaOut[0]) != "fake-mp3" {
		t.Errorf("expected audio reply, got %+v", sender.mediaOut)
	}
	if ttsSvc.lastText != "respuesta hablada" {
		t.Errorf("TTS received wrong text: %s", ttsSvc.lastText)
	}
}
