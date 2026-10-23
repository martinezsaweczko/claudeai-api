// Package weather implements the "tiempo:" and "prevision:" commands: fetches
// OpenWeatherMap data and asks the AI to write a friendly weather note.
package weather

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Completer produces an AI completion (consumer-defined interface)
type Completer interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// Service provides weather reports
type Service struct {
	client      *http.Client
	apiURL      string
	forecastURL string
	apiKey      string
	noteWords   int
	ai          Completer
	log         *slog.Logger
	tracer      trace.Tracer
}

// New creates the weather service
func New(client *http.Client, apiURL, forecastURL, apiKey string, noteWords int, ai Completer, log *slog.Logger, tp trace.TracerProvider) *Service {
	return &Service{
		client:      client,
		apiURL:      apiURL,
		forecastURL: forecastURL,
		apiKey:      apiKey,
		noteWords:   noteWords,
		ai:          ai,
		log:         log,
		tracer:      tp.Tracer("services/weather"),
	}
}

// fetch calls an OpenWeatherMap endpoint and returns the raw response body
func (s *Service) fetch(ctx context.Context, apiURL string, params url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build weather request: %w", err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("weather request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("weather API returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read weather response: %w", err)
	}

	return body, nil
}

// Report builds the friendly weather note for a city
func (s *Service) Report(ctx context.Context, city string) (string, error) {
	ctx, span := s.tracer.Start(ctx, "weather.Report")
	defer span.End()

	raw, err := s.fetch(ctx, s.apiURL, url.Values{
		"q":       {city},
		"units":   {"metric"},
		"appid":   {s.apiKey},
		"exclude": {"current,minutely,hourly,alerts,daily,"},
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	prompt := fmt.Sprintf("Por favor escribe una nota metereologica en %d palabras con iconos de whatsapp de forma amistosa y alegre segun este json de open weather. Indica la ciudad y pais de la ciudad: %s",
		s.noteWords, string(raw))

	note, err := s.ai.Complete(ctx, prompt)
	if err != nil {
		err = fmt.Errorf("failed to generate weather note: %w", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	return note, nil
}

// forecastSlots is the number of 3-hour slots requested (24 hours)
const forecastSlots = "8"

// forecastResponse is the subset of the OpenWeatherMap 5 day / 3 hour forecast
// used to build the prompt. "cod" is left out on purpose: it is a string in
// this endpoint and a number in the current-weather one.
type forecastResponse struct {
	List []forecastSlot `json:"list"`
	City struct {
		Name     string `json:"name"`
		Country  string `json:"country"`
		Timezone int    `json:"timezone"` // Shift from UTC in seconds
	} `json:"city"`
}

// forecastSlot is one 3-hour step of the forecast
type forecastSlot struct {
	Dt   int64 `json:"dt"` // Unix time, UTC
	Main struct {
		Temp float64 `json:"temp"`
	} `json:"main"`
	Weather []struct {
		Description string `json:"description"`
	} `json:"weather"`
	Wind struct {
		Speed float64 `json:"speed"` // m/s with units=metric
	} `json:"wind"`
	Pop float64 `json:"pop"` // Probability of precipitation, 0 to 1
	// Rain is absent when no rain is expected
	Rain *struct {
		ThreeH float64 `json:"3h"` // mm in the 3-hour slot
	} `json:"rain"`
}

// line renders the slot as one labeled line in the given local time zone
func (f forecastSlot) line(loc *time.Location) string {
	parts := []string{
		time.Unix(f.Dt, 0).In(loc).Format("15:04"),
		fmt.Sprintf("%.1f°C", f.Main.Temp),
	}
	if len(f.Weather) > 0 && f.Weather[0].Description != "" {
		parts = append(parts, f.Weather[0].Description)
	}
	parts = append(parts, fmt.Sprintf("prob. lluvia %.0f%%", math.Round(f.Pop*100)))
	if f.Rain != nil && f.Rain.ThreeH > 0 {
		parts = append(parts, fmt.Sprintf("%.1f mm", f.Rain.ThreeH))
	}
	// m/s to km/h
	parts = append(parts, fmt.Sprintf("viento %.0f km/h", math.Round(f.Wind.Speed*3.6)))

	return strings.Join(parts, " | ")
}

// Forecast builds the friendly note about the next 24 hours for a city
func (s *Service) Forecast(ctx context.Context, city string) (string, error) {
	ctx, span := s.tracer.Start(ctx, "weather.Forecast")
	defer span.End()

	fail := func(err error) (string, error) {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	raw, err := s.fetch(ctx, s.forecastURL, url.Values{
		"q":     {city},
		"units": {"metric"},
		"lang":  {"es"},
		"cnt":   {forecastSlots},
		"appid": {s.apiKey},
	})
	if err != nil {
		return fail(err)
	}

	var forecast forecastResponse
	if err := json.Unmarshal(raw, &forecast); err != nil {
		return fail(fmt.Errorf("failed to decode forecast response: %w", err))
	}
	if len(forecast.List) == 0 {
		return fail(fmt.Errorf("forecast response has no slots"))
	}

	// Slots are in UTC; the city's shift gives its local time without a tz database
	loc := time.FixedZone(forecast.City.Name, forecast.City.Timezone)
	lines := make([]string, 0, len(forecast.List))
	for _, slot := range forecast.List {
		lines = append(lines, slot.line(loc))
	}

	prompt := fmt.Sprintf("Escribe una previsión meteorológica en %d palabras, con iconos de whatsapp, de forma amistosa y alegre, para las próximas 24 horas en %s (%s). Los datos son tramos de 3 horas en hora local. Resume la evolución del día y destaca los cambios importantes (lluvia, viento, temperatura).\n\n%s",
		s.noteWords, forecast.City.Name, forecast.City.Country, strings.Join(lines, "\n"))

	note, err := s.ai.Complete(ctx, prompt)
	if err != nil {
		return fail(fmt.Errorf("failed to generate forecast note: %w", err))
	}

	return note, nil
}

// Commands returns the WhatsApp commands of the weather service
func (s *Service) Commands() []whatsapp.Command {
	return []whatsapp.Command{
		{Name: "weather", Pattern: regexp.MustCompile(`^(?i)tiempo:\s*`), Handler: s.weatherHandler()},
		{Name: "forecast", Pattern: regexp.MustCompile(`^(?i)previsi[oó]n:\s*`), Handler: s.forecastHandler()},
	}
}

var (
	// weatherPrefix strips the "tiempo:" prefix
	weatherPrefix = regexp.MustCompile(`^(?i)tiempo:`)
	// forecastPrefix strips the "prevision:" prefix (with or without accent)
	forecastPrefix = regexp.MustCompile(`^(?i)previsi[oó]n:`)
)

// cityCommand describes a command that takes a city and replies with a note
type cityCommand struct {
	label   string         // Used in logs ("Weather", "Forecast")
	prefix  *regexp.Regexp // Command prefix stripped from the message body
	usage   string         // Reply when no city is given
	failure string         // Reply prefix when the note cannot be built; the city is appended
	note    func(ctx context.Context, city string) (string, error)
}

func (s *Service) weatherHandler() whatsapp.CommandHandler {
	return s.cityHandler(cityCommand{
		label:   "Weather",
		prefix:  weatherPrefix,
		usage:   "Indica la ciudad. Ejemplo: *_tiempo: Madrid_*",
		failure: "No he podido obtener el tiempo para ",
		note:    s.Report,
	})
}

func (s *Service) forecastHandler() whatsapp.CommandHandler {
	return s.cityHandler(cityCommand{
		label:   "Forecast",
		prefix:  forecastPrefix,
		usage:   "Indica la ciudad. Ejemplo: *_prevision: Madrid_*",
		failure: "No he podido obtener la previsión para ",
		note:    s.Forecast,
	})
}

// cityHandler builds the handler shared by the city commands
func (s *Service) cityHandler(cmd cityCommand) whatsapp.CommandHandler {
	return func(ctx context.Context, sender whatsapp.Sender, msg whatsapp.IncomingMessage) error {
		city := strings.TrimSpace(cmd.prefix.ReplaceAllString(msg.Body, ""))
		s.log.Info(cmd.label+" requested", "user", msg.PushName, "city", city)

		if city == "" {
			return sender.ReplyText(ctx, msg, cmd.usage)
		}

		note, err := cmd.note(ctx, city)
		if err != nil {
			s.log.Error(cmd.label+" report failed", "city", city, "error", err)
			return sender.ReplyText(ctx, msg, cmd.failure+city)
		}

		return sender.ReplyText(ctx, msg, note)
	}
}
