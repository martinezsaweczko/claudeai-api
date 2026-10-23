package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

type fakeNotificationSender struct {
	calls []struct {
		To   types.JID
		Text string
	}
	err error
}

func (f *fakeNotificationSender) SendText(_ context.Context, to types.JID, text string) error {
	f.calls = append(f.calls, struct {
		To   types.JID
		Text string
	}{To: to, Text: text})
	return f.err
}

func newNotificationHandler(t *testing.T, sender *fakeNotificationSender) *NotificationHandler {
	t.Helper()
	h, err := (NotificationHandlerConfig{
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sender: sender,
	}).NewNotificationHandler()
	if err != nil {
		t.Fatalf("failed to create handler: %v", err)
	}
	return h
}

func TestNotificationHandlerSend(t *testing.T) {
	sender := &fakeNotificationSender{}
	h := newNotificationHandler(t, sender)

	body := `{"recipient":"15551234567","message":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.send(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if len(sender.calls) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sender.calls))
	}
	wantJID := types.NewJID("15551234567", types.DefaultUserServer)
	if sender.calls[0].To != wantJID {
		t.Errorf("recipient JID = %v, want %v", sender.calls[0].To, wantJID)
	}
	if sender.calls[0].Text != "hello" {
		t.Errorf("message = %q, want %q", sender.calls[0].Text, "hello")
	}
}

func TestNotificationHandlerFullJID(t *testing.T) {
	sender := &fakeNotificationSender{}
	h := newNotificationHandler(t, sender)

	body := `{"recipient":"120363420531996267@g.us","message":"group hello"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.send(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if len(sender.calls) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sender.calls))
	}
	wantJID := types.NewJID("120363420531996267", types.GroupServer)
	if sender.calls[0].To != wantJID {
		t.Errorf("recipient JID = %v, want %v", sender.calls[0].To, wantJID)
	}
}

func TestNotificationHandlerValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"invalid JSON", `{`},
		{"missing message", `{"recipient":"15551234567"}`},
		{"missing recipient", `{"message":"hello"}`},
		{"empty fields", `{"recipient":"","message":""}`},
		{"unknown field", `{"recipient":"15551234567","message":"hello","extra":"x"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sender := &fakeNotificationSender{}
			h := newNotificationHandler(t, sender)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			h.send(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if len(sender.calls) != 0 {
				t.Error("sender should not have been called")
			}
		})
	}
}

func TestNotificationHandlerSendError(t *testing.T) {
	sender := &fakeNotificationSender{err: errors.New("send failed")}
	h := newNotificationHandler(t, sender)

	body := `{"recipient":"15551234567","message":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.send(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

func TestNotificationHandlerInvalidJID(t *testing.T) {
	sender := &fakeNotificationSender{}
	h := newNotificationHandler(t, sender)

	body := `{"recipient":"not-a-jid","message":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.send(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
