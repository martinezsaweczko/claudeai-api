package tts

import (
	"log/slog"
	"net/http"
	"os"
	"testing"

	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func TestAudioCommandPattern(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc, err := New(http.DefaultClient, log, tracenoop.NewTracerProvider(), noop.NewMeterProvider())
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
		{name: "lowercase prefix", body: "audio: hola", want: true},
		{name: "capitalized prefix", body: "Audio: hola", want: true},
		{name: "no space after colon", body: "audio:hola", want: true},
		{name: "missing first letter", body: "udio: hola", want: false},
		{name: "missing colon", body: "audio hola", want: false},
		{name: "prefix not at start", body: "manda audio: hola", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pattern.MatchString(tt.body); got != tt.want {
				t.Errorf("Pattern.MatchString(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestAudioPrefixStripped(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "lowercase prefix", body: "audio: hola", want: " hola"},
		{name: "capitalized prefix", body: "Audio:hola", want: "hola"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := audioPrefix.ReplaceAllString(tt.body, ""); got != tt.want {
				t.Errorf("audioPrefix strip of %q = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}
