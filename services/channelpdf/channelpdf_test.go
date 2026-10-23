package channelpdf

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
	"go.mau.fi/whatsmeow/types"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

type fakeDownloader struct {
	data []byte
	err  error
	msgs []whatsapp.IncomingMessage
}

func (f *fakeDownloader) DownloadMedia(_ context.Context, msg whatsapp.IncomingMessage) ([]byte, error) {
	f.msgs = append(f.msgs, msg)
	return f.data, f.err
}

type fakeNotifier struct {
	calls []notifyCall
	err   error
}

type notifyCall struct {
	Category string
	Filename string
}

func (f *fakeNotifier) Notify(_ context.Context, _ whatsapp.Sender, category, filename string) error {
	f.calls = append(f.calls, notifyCall{Category: category, Filename: filename})
	return f.err
}

type fakeSender struct{}

func (f *fakeSender) ReplyText(_ context.Context, _ whatsapp.IncomingMessage, _ string) error {
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

func testService(t *testing.T, folder string) *Service {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc, err := New(
		"120363420531996267@g.us",
		"nacional",
		map[string]string{"nacional": folder},
		&fakeDownloader{data: []byte("pdf content")},
		&fakeNotifier{},
		log,
		tracenoop.NewTracerProvider(),
		noop.NewMeterProvider(),
	)
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}
	return svc
}

func TestNewValidations(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_, err := New("", "nacional", map[string]string{"nacional": "/tmp"}, nil, nil, log, tracenoop.NewTracerProvider(), noop.NewMeterProvider())
	if err == nil {
		t.Error("expected error for empty channel JID")
	}

	_, err = New("invalid", "nacional", map[string]string{"nacional": "/tmp"}, nil, nil, log, tracenoop.NewTracerProvider(), noop.NewMeterProvider())
	if err == nil {
		t.Error("expected error for invalid channel JID")
	}

	_, err = New("120363420531996267@g.us", "unknown", map[string]string{"nacional": "/tmp"}, nil, nil, log, tracenoop.NewTracerProvider(), noop.NewMeterProvider())
	if err == nil {
		t.Error("expected error for unknown category")
	}

	_, err = New("120363420531996267@g.us", "nacional", map[string]string{"nacional": ""}, nil, nil, log, tracenoop.NewTracerProvider(), noop.NewMeterProvider())
	if err == nil {
		t.Error("expected error for empty category directory")
	}
}

func mustParseJID(t *testing.T, s string) types.JID {
	t.Helper()
	jid, err := types.ParseJID(s)
	if err != nil {
		t.Fatalf("invalid JID %q: %v", s, err)
	}
	return jid
}

func TestHandlerSavesAndNotifies(t *testing.T) {
	folder := t.TempDir()
	svc := testService(t, folder)
	svc.downloader = &fakeDownloader{data: []byte("pdf content")}
	notifier := &fakeNotifier{}
	svc.notifier = notifier

	msg := whatsapp.IncomingMessage{
		Chat:             mustParseJID(t, "120363420531996267@g.us"),
		HasDocument:      true,
		DocumentMIMEType: "application/pdf",
		DocumentFileName: "paper.pdf",
	}

	err := svc.Handler()(context.Background(), &fakeSender{}, msg)
	if err != nil {
		t.Fatalf("handler failed: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(folder, "paper.pdf"))
	if err != nil {
		t.Fatalf("saved file not found: %v", err)
	}
	if string(content) != "pdf content" {
		t.Errorf("saved content = %q, want %q", string(content), "pdf content")
	}

	if len(notifier.calls) != 1 || notifier.calls[0].Category != "nacional" || notifier.calls[0].Filename != "paper.pdf" {
		t.Errorf("notifier calls = %v, want [{nacional paper.pdf}]", notifier.calls)
	}
}

func TestHandlerSkipsExistingFile(t *testing.T) {
	folder := t.TempDir()
	svc := testService(t, folder)

	existingPath := filepath.Join(folder, "paper.pdf")
	if err := os.WriteFile(existingPath, []byte("existing"), 0o644); err != nil {
		t.Fatalf("failed to create existing file: %v", err)
	}

	notifier := &fakeNotifier{}
	svc.notifier = notifier

	msg := whatsapp.IncomingMessage{
		Chat:             mustParseJID(t, "120363420531996267@g.us"),
		HasDocument:      true,
		DocumentMIMEType: "application/pdf",
		DocumentFileName: "paper.pdf",
	}

	err := svc.Handler()(context.Background(), &fakeSender{}, msg)
	if err != nil {
		t.Fatalf("handler failed: %v", err)
	}

	content, _ := os.ReadFile(existingPath)
	if string(content) != "existing" {
		t.Error("existing file should not have been overwritten")
	}

	if len(notifier.calls) != 0 {
		t.Error("subscribers should not be notified for an existing file")
	}
}

func TestHandlerIgnoresOtherJID(t *testing.T) {
	folder := t.TempDir()
	svc := testService(t, folder)
	notifier := &fakeNotifier{}
	svc.notifier = notifier

	msg := whatsapp.IncomingMessage{
		Chat:             mustParseJID(t, "99999@g.us"),
		HasDocument:      true,
		DocumentMIMEType: "application/pdf",
		DocumentFileName: "paper.pdf",
	}

	err := svc.Handler()(context.Background(), &fakeSender{}, msg)
	if err != nil {
		t.Fatalf("handler failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(folder, "paper.pdf")); !os.IsNotExist(err) {
		t.Error("file should not be saved for a different JID")
	}
	if len(notifier.calls) != 0 {
		t.Error("subscribers should not be notified for a different JID")
	}
}

func TestHandlerIgnoresNonDocuments(t *testing.T) {
	folder := t.TempDir()
	svc := testService(t, folder)
	notifier := &fakeNotifier{}
	svc.notifier = notifier

	msg := whatsapp.IncomingMessage{
		Chat:        mustParseJID(t, "120363420531996267@g.us"),
		HasDocument: false,
	}

	err := svc.Handler()(context.Background(), &fakeSender{}, msg)
	if err != nil {
		t.Fatalf("handler failed: %v", err)
	}

	if len(notifier.calls) != 0 {
		t.Error("subscribers should not be notified for non-documents")
	}
}

func TestHandlerSkipsEmptyFilename(t *testing.T) {
	folder := t.TempDir()
	svc := testService(t, folder)
	notifier := &fakeNotifier{}
	svc.notifier = notifier

	msg := whatsapp.IncomingMessage{
		Chat:             mustParseJID(t, "120363420531996267@g.us"),
		HasDocument:      true,
		DocumentMIMEType: "application/pdf",
		DocumentFileName: "",
	}

	err := svc.Handler()(context.Background(), &fakeSender{}, msg)
	if err != nil {
		t.Fatalf("handler failed: %v", err)
	}

	if len(notifier.calls) != 0 {
		t.Error("subscribers should not be notified when filename is empty")
	}
}

func TestHandlerReturnsDownloadError(t *testing.T) {
	folder := t.TempDir()
	svc := testService(t, folder)
	svc.downloader = &fakeDownloader{err: errors.New("download failed")}
	notifier := &fakeNotifier{}
	svc.notifier = notifier

	msg := whatsapp.IncomingMessage{
		Chat:             mustParseJID(t, "120363420531996267@g.us"),
		HasDocument:      true,
		DocumentMIMEType: "application/pdf",
		DocumentFileName: "paper.pdf",
	}

	err := svc.Handler()(context.Background(), &fakeSender{}, msg)
	if err == nil {
		t.Fatal("expected error when download fails")
	}

	if len(notifier.calls) != 0 {
		t.Error("subscribers should not be notified when download fails")
	}
}

func TestHandlerReturnsNotifyError(t *testing.T) {
	folder := t.TempDir()
	svc := testService(t, folder)
	notifier := &fakeNotifier{err: errors.New("notify failed")}
	svc.notifier = notifier

	msg := whatsapp.IncomingMessage{
		Chat:             mustParseJID(t, "120363420531996267@g.us"),
		HasDocument:      true,
		DocumentMIMEType: "application/pdf",
		DocumentFileName: "paper.pdf",
	}

	err := svc.Handler()(context.Background(), &fakeSender{}, msg)
	if err == nil {
		t.Fatal("expected error when notification fails")
	}

	content, err := os.ReadFile(filepath.Join(folder, "paper.pdf"))
	if err != nil {
		t.Fatalf("saved file not found: %v", err)
	}
	if string(content) != "pdf content" {
		t.Errorf("saved content = %q, want %q", string(content), "pdf content")
	}
}
