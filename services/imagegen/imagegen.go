// Package imagegen implements the "imagen:" command via the Stability AI API.
package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"

	"github.com/martinezsaweczko/whatsappBot-golang/o11"
	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const endpoint = "https://api.stability.ai/v2beta/stable-image/generate/core"

// Service provides AI image generation
type Service struct {
	client      *http.Client
	apiKey      string
	log         *slog.Logger
	tracer      trace.Tracer
	imagesTotal metric.Int64Counter
}

// New creates the image generation service
func New(client *http.Client, apiKey string, log *slog.Logger, tp trace.TracerProvider, mp metric.MeterProvider) (*Service, error) {
	imagesTotal, err := o11.NewBusinessCounter(mp, "images_generated_total", "Images generated")
	if err != nil {
		return nil, fmt.Errorf("failed to create images counter: %w", err)
	}

	return &Service{
		client:      client,
		apiKey:      apiKey,
		log:         log,
		tracer:      tp.Tracer("services/imagegen"),
		imagesTotal: imagesTotal,
	}, nil
}

// stabilityResponse is the JSON response of the Stability API
type stabilityResponse struct {
	Image        string `json:"image"`
	FinishReason string `json:"finish_reason"`
	Seed         int64  `json:"seed"`
}

// Generate creates a JPEG image for the given prompt
func (s *Service) Generate(ctx context.Context, prompt string) ([]byte, error) {
	ctx, span := s.tracer.Start(ctx, "imagegen.Generate")
	defer span.End()

	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		err := fmt.Errorf("indica el texto de la imagen. Ejemplo: *_imagen: un gato en el espacio_*")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("prompt", prompt); err != nil {
		err = fmt.Errorf("failed to build request: %w", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	if err := writer.WriteField("output_format", "jpeg"); err != nil {
		err = fmt.Errorf("failed to build request: %w", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	if err := writer.Close(); err != nil {
		err = fmt.Errorf("failed to build request: %w", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		err = fmt.Errorf("failed to build request: %w", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := s.client.Do(req)
	if err != nil {
		err = fmt.Errorf("stability request failed: %w", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		err = fmt.Errorf("failed to read stability response: %w", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("stability API returned status %d: %s", resp.StatusCode, string(respBody))
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	var parsed stabilityResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		err = fmt.Errorf("failed to parse stability response: %w", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	image, err := base64.StdEncoding.DecodeString(parsed.Image)
	if err != nil {
		err = fmt.Errorf("failed to decode generated image: %w", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	s.imagesTotal.Add(ctx, 1)
	return image, nil
}

// Commands returns the WhatsApp commands of the image generation service
func (s *Service) Commands() []whatsapp.Command {
	return []whatsapp.Command{
		{Name: "imagen", Pattern: regexp.MustCompile(`^(?i)imagen:\s*`), Handler: s.imagenHandler()},
	}
}

// imagenPrefix strips the "imagen:" prefix
var imagenPrefix = regexp.MustCompile(`^(?i)imagen:`)

func (s *Service) imagenHandler() whatsapp.CommandHandler {
	return func(ctx context.Context, sender whatsapp.Sender, msg whatsapp.IncomingMessage) error {
		prompt := strings.TrimSpace(imagenPrefix.ReplaceAllString(msg.Body, ""))
		s.log.Info("Image generation requested", "user", msg.PushName)

		image, err := s.Generate(ctx, prompt)
		if err != nil {
			s.log.Error("Image generation failed", "error", err)
			return sender.ReplyText(ctx, msg, "No he podido generar la imagen: "+err.Error())
		}

		return sender.ReplyMedia(ctx, msg, image, "image/jpeg", "imagen.jpg")
	}
}
