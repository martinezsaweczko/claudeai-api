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

	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
	"go.mau.fi/whatsmeow/types"
)

type fakeGroupClient struct {
	groups       []whatsapp.Group
	participants []whatsapp.GroupParticipant
	err          error
	lastJID      types.JID
}

func (f *fakeGroupClient) GetJoinedGroups(_ context.Context) ([]whatsapp.Group, error) {
	return f.groups, f.err
}

func (f *fakeGroupClient) GetGroupParticipants(_ context.Context, groupJID types.JID) ([]whatsapp.GroupParticipant, error) {
	f.lastJID = groupJID
	return f.participants, f.err
}

func newWhatsAppHandler(t *testing.T, client *fakeGroupClient) *WhatsAppHandler {
	t.Helper()
	h, err := (WhatsAppHandlerConfig{
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		GroupClient: client,
		BasePath:    "/api/v1",
	}).NewWhatsAppHandler()
	if err != nil {
		t.Fatalf("failed to create handler: %v", err)
	}
	return h
}

func TestWhatsAppListGroups(t *testing.T) {
	client := &fakeGroupClient{
		groups: []whatsapp.Group{
			{JID: types.NewJID("120363420531996267", types.GroupServer), Name: "News"},
		},
	}
	h := newWhatsAppHandler(t, client)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/whatsapp/groups", nil)
	rec := httptest.NewRecorder()
	h.listGroups(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), `"jid":"120363420531996267@g.us"`) {
		t.Errorf("response does not contain expected JID: %s", rec.Body.String())
	}
}

func TestWhatsAppListGroupsError(t *testing.T) {
	client := &fakeGroupClient{err: errors.New("failed")}
	h := newWhatsAppHandler(t, client)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/whatsapp/groups", nil)
	rec := httptest.NewRecorder()
	h.listGroups(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

func TestWhatsAppListParticipants(t *testing.T) {
	client := &fakeGroupClient{
		participants: []whatsapp.GroupParticipant{
			{JID: types.NewJID("34645568517", types.DefaultUserServer), DisplayName: "David", IsAdmin: true},
		},
	}
	h := newWhatsAppHandler(t, client)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/whatsapp/groups/120363420531996267@g.us/participants", nil)
	req.SetPathValue("jid", "120363420531996267@g.us")
	rec := httptest.NewRecorder()
	h.listParticipants(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), `"jid":"34645568517@s.whatsapp.net"`) {
		t.Errorf("response does not contain expected JID: %s", rec.Body.String())
	}
	if client.lastJID != types.NewJID("120363420531996267", types.GroupServer) {
		t.Errorf("lastJID = %v, want 120363420531996267@g.us", client.lastJID)
	}
}

func TestWhatsAppListParticipantsInvalidJID(t *testing.T) {
	client := &fakeGroupClient{}
	h := newWhatsAppHandler(t, client)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/whatsapp/groups/invalid/participants", nil)
	req.SetPathValue("jid", "invalid")
	rec := httptest.NewRecorder()
	h.listParticipants(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestWhatsAppListParticipantsError(t *testing.T) {
	client := &fakeGroupClient{err: errors.New("failed")}
	h := newWhatsAppHandler(t, client)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/whatsapp/groups/120363420531996267@g.us/participants", nil)
	req.SetPathValue("jid", "120363420531996267@g.us")
	rec := httptest.NewRecorder()
	h.listParticipants(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}
