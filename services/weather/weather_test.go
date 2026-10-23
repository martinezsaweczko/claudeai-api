package weather

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
	"go.mau.fi/whatsmeow/types"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

type fakeCompleter struct {
	lastPrompt string
	reply      string
	err        error
}

func (f *fakeCompleter) Complete(_ context.Context, prompt string) (string, error) {
	f.lastPrompt = prompt
	return f.reply, f.err
}

func TestReport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("appid") != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("q") != "Madrid" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"name":"Madrid","main":{"temp":25.5},"sys":{"country":"ES"}}`))
	}))
	defer server.Close()

	ai := &fakeCompleter{reply: "Día soleado en Madrid ☀️"}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := New(server.Client(), server.URL, server.URL, "test-key", 200, ai, log, tracenoop.NewTracerProvider())

	note, err := svc.Report(context.Background(), "Madrid")
	if err != nil {
		t.Fatalf("Report failed: %v", err)
	}
	if note != "Día soleado en Madrid ☀️" {
		t.Errorf("unexpected note: %s", note)
	}
	// The AI prompt must contain the raw weather JSON
	if !strings.Contains(ai.lastPrompt, "Madrid") || !strings.Contains(ai.lastPrompt, "25.5") {
		t.Errorf("prompt missing weather data: %s", ai.lastPrompt)
	}
	if !strings.Contains(ai.lastPrompt, "200 palabras") {
		t.Errorf("prompt missing word count: %s", ai.lastPrompt)
	}
}

func TestReportAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	ai := &fakeCompleter{}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := New(server.Client(), server.URL, server.URL, "key", 200, ai, log, tracenoop.NewTracerProvider())

	if _, err := svc.Report(context.Background(), "Nowhere"); err == nil {
		t.Fatal("expected error for API failure")
	}
}

// TestReportRequestParams guards the current-weather request against changes
// in the shared fetch code
func TestReportRequestParams(t *testing.T) {
	var got url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		w.Write([]byte(`{"name":"Madrid"}`))
	}))
	defer server.Close()

	svc := newTestService(server, "http://forecast.invalid", &fakeCompleter{reply: "ok"})
	if _, err := svc.Report(context.Background(), "Madrid"); err != nil {
		t.Fatalf("Report failed: %v", err)
	}

	want := url.Values{
		"q":       {"Madrid"},
		"units":   {"metric"},
		"appid":   {"test-key"},
		"exclude": {"current,minutely,hourly,alerts,daily,"},
	}
	if got.Encode() != want.Encode() {
		t.Errorf("unexpected weather params: got %q, want %q", got.Encode(), want.Encode())
	}
}

// fakeSender records the text replies of the command handlers
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

// forecastJSON is a trimmed OpenWeather 5 day / 3 hour response. The first slot
// is 15:00 UTC and the city is at UTC+2, so local hours are 17:00, 20:00 and 23:00.
// The second slot has no "rain" object and the third has an empty "weather" array.
const forecastJSON = `{"cod":"200","cnt":3,"list":[
{"dt":1661871600,"main":{"temp":23.6,"feels_like":23.8,"humidity":69},"weather":[{"id":500,"main":"Rain","description":"lluvia ligera","icon":"10d"}],"wind":{"speed":0.62,"deg":349,"gust":1.18},"pop":0.32,"rain":{"3h":0.26},"sys":{"pod":"d"},"dt_txt":"2022-08-30 15:00:00"},
{"dt":1661882400,"main":{"temp":20},"weather":[{"id":803,"main":"Clouds","description":"nubes","icon":"04n"}],"wind":{"speed":5},"pop":0,"dt_txt":"2022-08-30 18:00:00"},
{"dt":1661893200,"main":{"temp":18.2},"weather":[],"wind":{"speed":1},"pop":0.1,"dt_txt":"2022-08-30 21:00:00"}
],"city":{"name":"Zocca","country":"IT","timezone":7200}}`

func newTestService(server *httptest.Server, forecastURL string, ai Completer) *Service {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
	return New(server.Client(), server.URL, forecastURL, "test-key", 200, ai, log, tracenoop.NewTracerProvider())
}

func TestForecast(t *testing.T) {
	var got url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		w.Write([]byte(forecastJSON))
	}))
	defer server.Close()

	ai := &fakeCompleter{reply: "Tarde de lluvia ligera 🌦️"}
	svc := newTestService(server, server.URL, ai)

	// Lowercase input: the prompt must name the city as resolved by the API
	note, err := svc.Forecast(context.Background(), "zocca")
	if err != nil {
		t.Fatalf("Forecast failed: %v", err)
	}
	if note != "Tarde de lluvia ligera 🌦️" {
		t.Errorf("unexpected note: %s", note)
	}

	wantParams := url.Values{
		"q":     {"zocca"},
		"units": {"metric"},
		"lang":  {"es"},
		"cnt":   {"8"},
		"appid": {"test-key"},
	}
	if got.Encode() != wantParams.Encode() {
		t.Errorf("unexpected forecast params: got %q, want %q", got.Encode(), wantParams.Encode())
	}

	wantLines := []string{
		"17:00 | 23.6°C | lluvia ligera | prob. lluvia 32% | 0.3 mm | viento 2 km/h",
		"20:00 | 20.0°C | nubes | prob. lluvia 0% | viento 18 km/h",
		"23:00 | 18.2°C | prob. lluvia 10% | viento 4 km/h",
	}
	header, data, found := strings.Cut(ai.lastPrompt, "\n\n")
	if !found {
		t.Fatalf("prompt has no blank line before the slots: %q", ai.lastPrompt)
	}
	if data != strings.Join(wantLines, "\n") {
		t.Errorf("unexpected slot lines:\ngot:\n%s\nwant:\n%s", data, strings.Join(wantLines, "\n"))
	}
	for _, want := range []string{"200 palabras", "próximas 24 horas en Zocca (IT)", "tramos de 3 horas en hora local"} {
		if !strings.Contains(header, want) {
			t.Errorf("prompt header missing %q: %s", want, header)
		}
	}
	if strings.Contains(ai.lastPrompt, "zocca") {
		t.Errorf("prompt uses the raw user input instead of the resolved city: %s", ai.lastPrompt)
	}
}

func TestForecastErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		aiErr  error
	}{
		{name: "non-200 status", status: http.StatusNotFound, body: `{"cod":"404","message":"city not found"}`},
		{name: "invalid JSON", status: http.StatusOK, body: `{"list":`},
		{name: "empty list", status: http.StatusOK, body: `{"cod":"200","list":[],"city":{"name":"Zocca","country":"IT"}}`},
		{name: "missing list", status: http.StatusOK, body: `{}`},
		{name: "AI failure", status: http.StatusOK, body: forecastJSON, aiErr: errors.New("ai down")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			}))
			defer server.Close()

			svc := newTestService(server, server.URL, &fakeCompleter{reply: "note", err: tt.aiErr})
			if note, err := svc.Forecast(context.Background(), "Zocca"); err == nil {
				t.Fatalf("expected error, got note %q", note)
			}
		})
	}
}

func TestCommandPatterns(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	svc := newTestService(server, server.URL, &fakeCompleter{})

	patterns := map[string]func(string) bool{}
	for _, cmd := range svc.Commands() {
		patterns[cmd.Name] = cmd.Pattern.MatchString
	}
	if patterns["weather"] == nil || patterns["forecast"] == nil {
		t.Fatalf("expected weather and forecast commands, got %d commands", len(patterns))
	}

	tests := []struct {
		body     string
		weather  bool
		forecast bool
	}{
		{body: "prevision: Madrid", forecast: true},
		{body: "previsión: Madrid", forecast: true},
		{body: "PREVISION: Madrid", forecast: true},
		{body: "PREVISIÓN: Madrid", forecast: true},
		{body: "prevision:Madrid", forecast: true},
		{body: "tiempo: Madrid", weather: true},
		{body: "TIEMPO: Madrid", weather: true},
		{body: "prevision Madrid"},
		{body: "la prevision: Madrid"},
	}

	for _, tt := range tests {
		t.Run(tt.body, func(t *testing.T) {
			if got := patterns["weather"](tt.body); got != tt.weather {
				t.Errorf("weather pattern match = %v, want %v", got, tt.weather)
			}
			if got := patterns["forecast"](tt.body); got != tt.forecast {
				t.Errorf("forecast pattern match = %v, want %v", got, tt.forecast)
			}
		})
	}
}

func TestForecastHandler(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		status int
		want   string
	}{
		{name: "empty city replies with the usage hint", body: "prevision:  ", status: http.StatusOK, want: "Indica la ciudad. Ejemplo: *_prevision: Madrid_*"},
		{name: "API failure replies with a friendly error", body: "previsión: Nowhere", status: http.StatusNotFound, want: "No he podido obtener la previsión para Nowhere"},
		{name: "success replies with the note", body: "PREVISIÓN: Zocca", status: http.StatusOK, want: "Tarde de lluvia ligera 🌦️"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(forecastJSON))
			}))
			defer server.Close()

			svc := newTestService(server, server.URL, &fakeCompleter{reply: "Tarde de lluvia ligera 🌦️"})
			var handler whatsapp.CommandHandler
			for _, cmd := range svc.Commands() {
				if cmd.Name == "forecast" {
					handler = cmd.Handler
				}
			}
			if handler == nil {
				t.Fatal("forecast command not registered")
			}

			sender := &fakeSender{}
			if err := handler(context.Background(), sender, whatsapp.IncomingMessage{Body: tt.body, PushName: "tester"}); err != nil {
				t.Fatalf("handler failed: %v", err)
			}
			if len(sender.replies) != 1 || sender.replies[0] != tt.want {
				t.Errorf("unexpected replies: got %q, want %q", sender.replies, tt.want)
			}
		})
	}
}

func TestWeatherHandler(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		status int
		want   string
	}{
		{name: "empty city replies with the usage hint", body: "tiempo:", status: http.StatusOK, want: "Indica la ciudad. Ejemplo: *_tiempo: Madrid_*"},
		{name: "API failure replies with a friendly error", body: "tiempo: Nowhere", status: http.StatusNotFound, want: "No he podido obtener el tiempo para Nowhere"},
		{name: "success replies with the note", body: "Tiempo: Madrid", status: http.StatusOK, want: "Día soleado"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(`{"name":"Madrid"}`))
			}))
			defer server.Close()

			svc := newTestService(server, server.URL, &fakeCompleter{reply: "Día soleado"})
			sender := &fakeSender{}
			handler := svc.Commands()[0].Handler
			if err := handler(context.Background(), sender, whatsapp.IncomingMessage{Body: tt.body, PushName: "tester"}); err != nil {
				t.Fatalf("handler failed: %v", err)
			}
			if len(sender.replies) != 1 || sender.replies[0] != tt.want {
				t.Errorf("unexpected replies: got %q, want %q", sender.replies, tt.want)
			}
		})
	}
}
