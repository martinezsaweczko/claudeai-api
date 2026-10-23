// Package tts implements text-to-speech via the Google Translate TTS endpoint
// (the same endpoint the Node gtts package uses).
package tts

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/martinezsaweczko/whatsappBot-golang/o11"
	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// endpoint is the Google Translate TTS URL (unofficial, same as npm gtts)
const endpoint = "https://translate.google.com/translate_tts"

// Service provides text-to-speech synthesis
type Service struct {
	client     *http.Client
	log        *slog.Logger
	tracer     trace.Tracer
	synthTotal metric.Int64Counter
}

// New creates the TTS service
func New(client *http.Client, log *slog.Logger, tp trace.TracerProvider, mp metric.MeterProvider) (*Service, error) {
	synthTotal, err := o11.NewBusinessCounter(mp, "tts_messages_total", "TTS audio messages generated")
	if err != nil {
		return nil, fmt.Errorf("failed to create tts counter: %w", err)
	}

	return &Service{
		client:     client,
		log:        log,
		tracer:     tp.Tracer("services/tts"),
		synthTotal: synthTotal,
	}, nil
}

// Synthesize converts text to MP3 audio (Spanish voice)
func (s *Service) Synthesize(ctx context.Context, text string) ([]byte, error) {
	ctx, span := s.tracer.Start(ctx, "tts.Synthesize")
	defer span.End()

	text = strings.TrimSpace(text)
	if text == "" {
		err := fmt.Errorf("no hay texto para convertir a audio")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	params := url.Values{
		"ie":      {"UTF-8"},
		"total":   {"1"},
		"idx":     {"0"},
		"client":  {"tw-ob"},
		"tl":      {"es"},
		"q":       {text},
		"textlen": {fmt.Sprintf("%d", len(text))},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+params.Encode(), nil)
	if err != nil {
		err = fmt.Errorf("failed to build TTS request: %w", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := s.client.Do(req)
	if err != nil {
		err = fmt.Errorf("TTS request failed: %w", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("TTS returned status %d", resp.StatusCode)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	audio, err := io.ReadAll(resp.Body)
	if err != nil {
		err = fmt.Errorf("failed to read TTS response: %w", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	s.synthTotal.Add(ctx, 1)
	return audio, nil
}

// Commands returns the WhatsApp commands of the TTS service
func (s *Service) Commands() []whatsapp.Command {
	return []whatsapp.Command{
		{Name: "audio", Pattern: regexp.MustCompile(`^(?i)audio:\s*`), Handler: s.audioHandler()},
	}
}

// audioPrefix strips the "audio:" prefix
var audioPrefix = regexp.MustCompile(`^(?i)audio:`)

// audioHandler converts the command text to speech, deletes the original
// command message and sends the audio to the chat
func (s *Service) audioHandler() whatsapp.CommandHandler {
	return func(ctx context.Context, sender whatsapp.Sender, msg whatsapp.IncomingMessage) error {
		s.log.Info("TTS requested", "user", msg.PushName)

		text := strings.TrimSpace(audioPrefix.ReplaceAllString(msg.Body, ""))
		audio, err := s.Synthesize(ctx, text)
		if err != nil {
			return sender.ReplyText(ctx, msg, "No he podido generar el audio: "+err.Error())
		}

		if err := sender.DeleteMessage(ctx, msg); err != nil {
			// Not fatal: the Node version also ignores deletion failures in practice
			s.log.Warn("Failed to delete original message", "error", err)
		}

		return sender.SendMedia(ctx, msg.Chat, audio, "audio/mpeg", "audio.mp3")
	}
}
