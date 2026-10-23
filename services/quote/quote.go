// Package quote implements the "quote" command: fetches a random quote from
// API Ninjas and adds a Spanish translation written by the AI.
package quote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Completer produces an AI completion (consumer-defined interface)
type Completer interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// Service provides random quotes
type Service struct {
	client *http.Client
	apiURL string
	apiKey string
	ai     Completer
	log    *slog.Logger
	tracer trace.Tracer
}

// New creates the quote service
func New(client *http.Client, apiURL, apiKey string, ai Completer, log *slog.Logger, tp trace.TracerProvider) *Service {
	return &Service{
		client: client,
		apiURL: apiURL,
		apiKey: apiKey,
		ai:     ai,
		log:    log,
		tracer: tp.Tracer("services/quote"),
	}
}

// apiQuote is one element of the API Ninjas random quotes response
type apiQuote struct {
	Quote  string `json:"quote"`
	Author string `json:"author"`
}

// fetch calls API Ninjas and returns the first quote of the response
func (s *Service) fetch(ctx context.Context) (apiQuote, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiURL, nil)
	if err != nil {
		return apiQuote{}, fmt.Errorf("failed to build quote request: %w", err)
	}
	req.Header.Set("X-Api-Key", s.apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return apiQuote{}, fmt.Errorf("quote request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apiQuote{}, fmt.Errorf("quote API returned status %d", resp.StatusCode)
	}

	var quotes []apiQuote
	if err := json.NewDecoder(resp.Body).Decode(&quotes); err != nil {
		return apiQuote{}, fmt.Errorf("failed to decode quote response: %w", err)
	}
	if len(quotes) == 0 {
		return apiQuote{}, errors.New("quote response has no quotes")
	}

	return quotes[0], nil
}

// clean collapses all whitespace (line breaks included) to single spaces and
// removes the WhatsApp formatting characters, so the text cannot break the
// bold and italic markup of the message
func clean(s string) string {
	s = strings.NewReplacer("*", "", "_", "").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// translationQuotes are the quotation marks models tend to wrap translations in
const translationQuotes = `"“”«»`

// cleanTranslation cleans the AI reply and strips surrounding quotation marks
func cleanTranslation(s string) string {
	return strings.TrimSpace(strings.Trim(clean(s), translationQuotes))
}

// format renders the WhatsApp message; author and translation are optional
func format(quote, author, translation string) string {
	var b strings.Builder
	b.WriteString("*“" + quote + "”*")
	if author != "" {
		b.WriteString("\n_— " + author + "_")
	}
	if translation != "" {
		b.WriteString("\n\n🇪🇸 " + translation)
	}
	return b.String()
}

// Random fetches a random quote and returns the message with its Spanish
// translation. A failed translation is not an error: the message then holds
// the English quote only.
func (s *Service) Random(ctx context.Context) (string, error) {
	ctx, span := s.tracer.Start(ctx, "quote.Random")
	defer span.End()

	raw, err := s.fetch(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	quote, author := clean(raw.Quote), clean(raw.Author)
	if quote == "" {
		err := errors.New("quote response has an empty quote")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	translation, err := s.translate(ctx, quote)
	if err != nil {
		// The command still succeeds, so the span status stays unset
		s.log.WarnContext(ctx, "Quote translation failed, sending the English quote only", "error", err)
		span.RecordError(err)
	}

	return format(quote, author, translation), nil
}

// translate asks the AI for the Spanish translation of the quote
func (s *Service) translate(ctx context.Context, quote string) (string, error) {
	prompt := "Translate this quote into Spanish. Reply only with the translation, " +
		"without quotation marks and without any comment: " + quote

	reply, err := s.ai.Complete(ctx, prompt)
	if err != nil {
		return "", fmt.Errorf("failed to translate quote: %w", err)
	}

	translation := cleanTranslation(reply)
	if translation == "" {
		return "", errors.New("quote translation is empty")
	}
	return translation, nil
}

// Commands returns the WhatsApp commands of the quote service
func (s *Service) Commands() []whatsapp.Command {
	return []whatsapp.Command{
		{Name: "quote", Pattern: regexp.MustCompile(`^(?i)quote\s*$`), Handler: s.quoteHandler()},
	}
}

// quoteHandler replies with a random quote. When the quote cannot be fetched
// it sends nothing and returns the error, which the router records.
func (s *Service) quoteHandler() whatsapp.CommandHandler {
	return func(ctx context.Context, sender whatsapp.Sender, msg whatsapp.IncomingMessage) error {
		s.log.InfoContext(ctx, "Quote requested", "user", msg.PushName)

		message, err := s.Random(ctx)
		if err != nil {
			s.log.ErrorContext(ctx, "Quote failed", "error", err)
			return err
		}

		return sender.ReplyText(ctx, msg, message)
	}
}
