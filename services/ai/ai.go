// Package ai implements the OpenAI fallback: messages that mention the bot
// and match no command are answered by an LLM (text or TTS audio).
package ai

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/martinezsaweczko/whatsappBot-golang/o11"
	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
	"github.com/sashabaranov/go-openai"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// trollPrompt is prepended to questions from troll-listed users
const trollPrompt = "Eres un bufon, responde lo que te preguntan pero intenta ser gracioso y troll, no des nunca una respuesta seria. Por favor no indiques que la respuesta forma parte de una broma, haz que no se note. Siempre intenta reirte de quien te pregunta."

// TTS synthesizes speech (consumer-defined interface)
type TTS interface {
	Synthesize(ctx context.Context, text string) ([]byte, error)
}

// Service provides AI completions
type Service struct {
	client       *openai.Client
	model        string
	trollJIDs    map[string]bool
	tts          TTS
	log          *slog.Logger
	tracer       trace.Tracer
	repliesTotal metric.Int64Counter
}

// New creates the AI service. httpClient is the instrumented external client.
func New(apiKey, model string, httpClient *http.Client, trollJIDs []string, tts TTS, log *slog.Logger, tp trace.TracerProvider, mp metric.MeterProvider) (*Service, error) {
	repliesTotal, err := o11.NewBusinessCounter(mp, "ai_replies_total", "AI replies per mode (text, audio)")
	if err != nil {
		return nil, fmt.Errorf("failed to create ai counter: %w", err)
	}

	cfg := openai.DefaultConfig(apiKey)
	cfg.HTTPClient = httpClient

	trolls := make(map[string]bool, len(trollJIDs))
	for _, j := range trollJIDs {
		trolls[j] = true
	}

	return &Service{
		client:       openai.NewClientWithConfig(cfg),
		model:        model,
		trollJIDs:    trolls,
		tts:          tts,
		log:          log,
		tracer:       tp.Tracer("services/ai"),
		repliesTotal: repliesTotal,
	}, nil
}

// Complete returns the chat completion for a prompt (shared with the weather service)
func (s *Service) Complete(ctx context.Context, prompt string) (string, error) {
	ctx, span := s.tracer.Start(ctx, "ai.Complete")
	defer span.End()

	resp, err := s.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: s.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleUser, Content: prompt},
		},
	})
	if err != nil {
		err = fmt.Errorf("OpenAI completion failed: %w", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}
	if len(resp.Choices) == 0 {
		err := fmt.Errorf("OpenAI returned no choices")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	return resp.Choices[0].Message.Content, nil
}

// audioAnswer matches GPT's classification answer ("audio" / "texto")
var audioAnswer = regexp.MustCompile(`^(?i)audio`)

// Fallback returns the handler for unmatched messages that mention the bot
func (s *Service) Fallback() whatsapp.CommandHandler {
	return func(ctx context.Context, sender whatsapp.Sender, msg whatsapp.IncomingMessage) error {
		s.log.Info("Processing AI fallback", "user", msg.PushName, "chat", msg.Chat.String())

		classification, err := s.Complete(ctx,
			"Responde con unica palabra, audio o texto, si la siguiente frase se refiere a un audio o a un texto: \""+msg.Body+"\"")
		if err != nil {
			classification = "texto"
			s.log.Warn("Audio/text classification failed, defaulting to text", "error", err)
		}

		if audioAnswer.MatchString(strings.TrimSpace(classification)) {
			return s.replyAudio(ctx, sender, msg)
		}
		return s.replyText(ctx, sender, msg)
	}
}

// replyText answers with a text completion (troll prompt when applicable)
func (s *Service) replyText(ctx context.Context, sender whatsapp.Sender, msg whatsapp.IncomingMessage) error {
	prompt := msg.Body
	if s.isTroll(msg) {
		prompt = trollPrompt + msg.Body
	}

	reply, err := s.Complete(ctx, prompt)
	if err != nil {
		return sender.ReplyText(ctx, msg, "Ahora mismo no puedo responder, inténtalo más tarde")
	}

	s.repliesTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("mode", "text")))
	return sender.ReplyText(ctx, msg, reply)
}

// replyAudio answers with a TTS audio of the completion
func (s *Service) replyAudio(ctx context.Context, sender whatsapp.Sender, msg whatsapp.IncomingMessage) error {
	reply, err := s.Complete(ctx, msg.Body)
	if err != nil {
		return sender.ReplyText(ctx, msg, "Ahora mismo no puedo responder, inténtalo más tarde")
	}

	audio, err := s.tts.Synthesize(ctx, reply)
	if err != nil {
		return sender.ReplyText(ctx, msg, "No he podido generar el audio de respuesta")
	}

	s.repliesTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("mode", "audio")))
	return sender.ReplyMedia(ctx, msg, audio, "audio/mpeg", "audio.mp3")
}

// isTroll reports whether the message author (or the group) is troll-listed
func (s *Service) isTroll(msg whatsapp.IncomingMessage) bool {
	return s.trollJIDs[msg.Sender.String()] || s.trollJIDs[msg.Chat.String()]
}
