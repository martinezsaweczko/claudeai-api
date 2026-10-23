package imagegen

import (
	"log/slog"
	"net/http"
	"os"
	"testing"

	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func TestImagenCommandPattern(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc, err := New(http.DefaultClient, "key", log, tracenoop.NewTracerProvider(), noop.NewMeterProvider())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cmds := svc.Commands()
	if len(cmds) != 1 {
		t.Fatalf("Commands() returned %d commands, want 1", len(cmds))
	}
	pattern := cmds[0].Pattern

	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "lowercase prefix", body: "imagen: un gato", want: true},
		{name: "capitalized prefix", body: "Imagen: un gato", want: true},
		{name: "no space after colon", body: "imagen:un gato", want: true},
		{name: "missing first letter", body: "magen: un gato", want: false},
		{name: "missing colon", body: "imagen un gato", want: false},
		{name: "prefix not at start", body: "una imagen: un gato", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pattern.MatchString(tt.body); got != tt.want {
				t.Errorf("Pattern.MatchString(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestImagenPrefixStripped(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "lowercase prefix", body: "imagen: un gato", want: " un gato"},
		{name: "capitalized prefix", body: "Imagen:un gato", want: "un gato"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := imagenPrefix.ReplaceAllString(tt.body, ""); got != tt.want {
				t.Errorf("imagenPrefix strip of %q = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}
