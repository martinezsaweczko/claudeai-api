package subscription

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/martinezsaweczko/whatsappBot-golang/model"
	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
	"go.mau.fi/whatsmeow/types"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// fakeStore is an in-memory Store that filters by user like the real
// repository and records the identity sets it receives
type fakeStore struct {
	saved   [][2]string
	deleted [][]string
	listed  [][]string
	subs    []model.Subscription
	matches []string
	err     error
}

func (f *fakeStore) SaveSubscription(_ context.Context, text, user string) error {
	f.saved = append(f.saved, [2]string{text, user})
	return f.err
}

func (f *fakeStore) DeleteSubscription(_ context.Context, users []string) error {
	f.deleted = append(f.deleted, users)
	if f.err != nil {
		return f.err
	}
	var kept []model.Subscription
	for _, sub := range f.subs {
		if !slices.Contains(users, sub.User) {
			kept = append(kept, sub)
		}
	}
	f.subs = kept
	return nil
}

func (f *fakeStore) ReturnSubscriptions(_ context.Context, users []string) ([]model.Subscription, error) {
	f.listed = append(f.listed, users)
	if f.err != nil {
		return nil, f.err
	}
	var subs []model.Subscription
	for _, sub := range f.subs {
		if slices.Contains(users, sub.User) {
			subs = append(subs, sub)
		}
	}
	return subs, nil
}

func (f *fakeStore) MatchSubscriptions(_ context.Context, _ string) ([]string, error) {
	return f.matches, f.err
}

type fakeLinkBuilder struct {
	expiry time.Duration
}

func (f *fakeLinkBuilder) GenerateSubscriptionToken(_ string, expiry time.Duration) (string, error) {
	f.expiry = expiry
	return "sub-token", nil
}

type fakeSender struct {
	sentTexts []struct {
		to   types.JID
		text string
	}
}

func (f *fakeSender) ReplyText(_ context.Context, _ whatsapp.IncomingMessage, _ string) error {
	return nil
}

func (f *fakeSender) SendText(_ context.Context, to types.JID, text string) error {
	f.sentTexts = append(f.sentTexts, struct {
		to   types.JID
		text string
	}{to, text})
	return nil
}

func (f *fakeSender) ReplyMedia(_ context.Context, _ whatsapp.IncomingMessage, _ []byte, _, _ string) error {
	return nil
}

func (f *fakeSender) SendMedia(_ context.Context, _ types.JID, _ []byte, _, _ string) error {
	return nil
}

func (f *fakeSender) DeleteMessage(_ context.Context, _ whatsapp.IncomingMessage) error {
	return nil
}

func newTestService(store *fakeStore) (*Service, *fakeLinkBuilder) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	jwt := &fakeLinkBuilder{}
	svc, err := New(store, jwt, "files.example.com:443",
		map[string]string{"nacional": "nacional_folder"},
		log, tracenoop.NewTracerProvider(), noop.NewMeterProvider())
	if err != nil {
		panic(err)
	}
	return svc, jwt
}

func testMessage() whatsapp.IncomingMessage {
	return whatsapp.IncomingMessage{
		Sender:   types.NewJID("34645568517", types.DefaultUserServer),
		PushName: "David",
	}
}

// Fake identities shared by the identity tests
const (
	testPhone      = "34600111222"
	testLID        = "111222333444555"
	testPhoneJID   = testPhone + "@s.whatsapp.net"
	testLegacyJID  = testPhone + "@c.us"
	testLIDJID     = testLID + "@lid"
	testOtherPhone = "34600999888@c.us"
)

func TestUserIdentities(t *testing.T) {
	phone := types.NewJID(testPhone, types.DefaultUserServer)
	legacy := types.NewJID(testPhone, types.LegacyUserServer)
	lid := types.NewJID(testLID, types.HiddenUserServer)
	withDevice := func(jid types.JID, device uint16) types.JID {
		jid.Device = device
		return jid
	}

	tests := []struct {
		name      string
		sender    types.JID
		senderAlt types.JID
		want      []string
	}{
		{
			name:   "phone number sender",
			sender: phone,
			want:   []string{testPhoneJID, testLegacyJID},
		},
		{
			name:   "phone number sender on a linked device",
			sender: withDevice(phone, 12),
			want:   []string{testPhoneJID, testLegacyJID},
		},
		{
			name:      "LID sender with phone number alternate",
			sender:    lid,
			senderAlt: phone,
			want:      []string{testPhoneJID, testLegacyJID, testLIDJID},
		},
		{
			name:      "phone number sender with LID alternate",
			sender:    phone,
			senderAlt: lid,
			want:      []string{testPhoneJID, testLegacyJID, testLIDJID},
		},
		{
			name:      "linked devices on both addresses",
			sender:    withDevice(lid, 12),
			senderAlt: withDevice(phone, 12),
			want:      []string{testPhoneJID, testLegacyJID, testLIDJID},
		},
		{
			name:   "LID sender without alternate",
			sender: lid,
			want:   []string{testLIDJID},
		},
		{
			name:   "LID sender on a linked device",
			sender: withDevice(lid, 12),
			want:   []string{testLIDJID},
		},
		{
			name:   "legacy server sender",
			sender: legacy,
			want:   []string{testPhoneJID, testLegacyJID},
		},
		{
			name:      "same phone number on both addresses",
			sender:    phone,
			senderAlt: legacy,
			want:      []string{testPhoneJID, testLegacyJID},
		},
		{
			name: "empty sender",
			want: nil,
		},
		{
			name:      "empty sender with phone number alternate",
			senderAlt: phone,
			want:      []string{testPhoneJID, testLegacyJID},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := userIdentities(whatsapp.IncomingMessage{Sender: tt.sender, SenderAlt: tt.senderAlt})
			if !slices.Equal(got, tt.want) {
				t.Errorf("userIdentities() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSubscribe(t *testing.T) {
	store := &fakeStore{}
	svc, _ := newTestService(store)

	if err := svc.Subscribe(context.Background(), testMessage(), "  El Mundo  "); err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	if len(store.saved) != 1 || store.saved[0] != [2]string{"El Mundo", testMessage().Sender.String()} {
		t.Errorf("unexpected saved subscription: %+v", store.saved)
	}
}

func TestSubscribeSavesUnderCanonicalIdentity(t *testing.T) {
	phone := types.NewJID(testPhone, types.DefaultUserServer)
	phoneDevice := phone
	phoneDevice.Device = 12
	lid := types.NewJID(testLID, types.HiddenUserServer)
	lidDevice := lid
	lidDevice.Device = 12

	tests := []struct {
		name string
		msg  whatsapp.IncomingMessage
		want string
	}{
		{"linked device", whatsapp.IncomingMessage{Sender: phoneDevice}, testPhoneJID},
		{"legacy server", whatsapp.IncomingMessage{Sender: types.NewJID(testPhone, types.LegacyUserServer)}, testPhoneJID},
		{"LID sender with phone number alternate", whatsapp.IncomingMessage{Sender: lid, SenderAlt: phone}, testPhoneJID},
		{"phone number sender with LID alternate", whatsapp.IncomingMessage{Sender: phone, SenderAlt: lid}, testPhoneJID},
		{"LID sender without alternate", whatsapp.IncomingMessage{Sender: lidDevice}, testLIDJID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{}
			svc, _ := newTestService(store)

			if err := svc.Subscribe(context.Background(), tt.msg, "Mundo"); err != nil {
				t.Fatalf("Subscribe failed: %v", err)
			}
			if len(store.saved) != 1 || store.saved[0] != [2]string{"Mundo", tt.want} {
				t.Errorf("saved = %+v, want one subscription under %q", store.saved, tt.want)
			}
		})
	}
}

func TestSubscribeWithoutSenderRejected(t *testing.T) {
	store := &fakeStore{}
	svc, _ := newTestService(store)

	if err := svc.Subscribe(context.Background(), whatsapp.IncomingMessage{}, "Mundo"); err == nil {
		t.Fatal("expected error for a message without sender")
	}
	if len(store.saved) != 0 {
		t.Errorf("nothing should be saved without a sender: %+v", store.saved)
	}
}

func TestSubscribeEmptyRejected(t *testing.T) {
	store := &fakeStore{}
	svc, _ := newTestService(store)
	if err := svc.Subscribe(context.Background(), testMessage(), "   "); err == nil {
		t.Fatal("expected error for empty subscription")
	}
}

func TestDelete(t *testing.T) {
	store := &fakeStore{}
	svc, _ := newTestService(store)

	if err := svc.Delete(context.Background(), testMessage()); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	want := []string{"34645568517@s.whatsapp.net", "34645568517@c.us"}
	if len(store.deleted) != 1 || !slices.Equal(store.deleted[0], want) {
		t.Errorf("deleted = %+v, want one call with %v", store.deleted, want)
	}
}

func TestDeletePassesAllIdentities(t *testing.T) {
	sender := types.NewJID(testPhone, types.DefaultUserServer)
	sender.Device = 12
	msg := whatsapp.IncomingMessage{Sender: sender, SenderAlt: types.NewJID(testLID, types.HiddenUserServer)}

	store := &fakeStore{subs: []model.Subscription{
		{SubscriptionText: "Mundo", User: testLegacyJID},
		{SubscriptionText: "Pais", User: testLIDJID},
		{SubscriptionText: "Marca", User: testPhoneJID},
		{SubscriptionText: "Economist", User: testOtherPhone},
	}}
	svc, _ := newTestService(store)

	if err := svc.Delete(context.Background(), msg); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	want := []string{testPhoneJID, testLegacyJID, testLIDJID}
	if len(store.deleted) != 1 || !slices.Equal(store.deleted[0], want) {
		t.Errorf("deleted = %+v, want one call with %v", store.deleted, want)
	}
	if len(store.subs) != 1 || store.subs[0].User != testOtherPhone {
		t.Errorf("only the other user's subscription should remain: %+v", store.subs)
	}
}

func TestList(t *testing.T) {
	user := testMessage().Sender.String()
	store := &fakeStore{subs: []model.Subscription{
		{ID: uuid.Must(uuid.NewV7()), SubscriptionText: "Mundo", User: user},
		{ID: uuid.Must(uuid.NewV7()), SubscriptionText: "Pais", User: user},
	}}
	svc, _ := newTestService(store)

	list, err := svc.List(context.Background(), testMessage())
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if !strings.Contains(list, "*Mundo*") || !strings.Contains(list, "*Pais*") {
		t.Errorf("list missing subscriptions: %s", list)
	}

	store.subs = nil
	list, _ = svc.List(context.Background(), testMessage())
	if list != "No tienes subscripciones activas" {
		t.Errorf("unexpected empty list message: %s", list)
	}
}

func TestListPassesAllIdentities(t *testing.T) {
	store := &fakeStore{}
	svc, _ := newTestService(store)
	msg := whatsapp.IncomingMessage{
		Sender:    types.NewJID(testLID, types.HiddenUserServer),
		SenderAlt: types.NewJID(testPhone, types.DefaultUserServer),
	}

	if _, err := svc.List(context.Background(), msg); err != nil {
		t.Fatalf("List failed: %v", err)
	}

	want := []string{testPhoneJID, testLegacyJID, testLIDJID}
	if len(store.listed) != 1 || !slices.Equal(store.listed[0], want) {
		t.Errorf("listed = %+v, want one call with %v", store.listed, want)
	}
}

// TestListFindsSubscriptionsStoredUnderEquivalentIdentity reproduces the
// production reports: rows imported from the Node app are keyed by a form of
// the user that whatsmeow never renders for an incoming message.
func TestListFindsSubscriptionsStoredUnderEquivalentIdentity(t *testing.T) {
	linkedDevice := types.NewJID(testPhone, types.DefaultUserServer)
	linkedDevice.Device = 12

	tests := []struct {
		name       string
		storedUser string
		msg        whatsapp.IncomingMessage
	}{
		{
			name:       "legacy server row and linked device sender",
			storedUser: testLegacyJID,
			msg:        whatsapp.IncomingMessage{Sender: linkedDevice},
		},
		{
			name:       "LID row and phone number sender with LID alternate",
			storedUser: testLIDJID,
			msg: whatsapp.IncomingMessage{
				Sender:    types.NewJID(testPhone, types.DefaultUserServer),
				SenderAlt: types.NewJID(testLID, types.HiddenUserServer),
			},
		},
		{
			name:       "legacy server row and LID sender with phone number alternate",
			storedUser: testLegacyJID,
			msg: whatsapp.IncomingMessage{
				Sender:    types.NewJID(testLID, types.HiddenUserServer),
				SenderAlt: types.NewJID(testPhone, types.DefaultUserServer),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{subs: []model.Subscription{
				{SubscriptionText: "Mundo", User: tt.storedUser},
				{SubscriptionText: "Economist", User: testOtherPhone},
			}}
			svc, _ := newTestService(store)

			list, err := svc.List(context.Background(), tt.msg)
			if err != nil {
				t.Fatalf("List failed: %v", err)
			}
			if list == "No tienes subscripciones activas" {
				t.Fatalf("subscription stored under %q was not found", tt.storedUser)
			}
			if !strings.Contains(list, "*Mundo*") {
				t.Errorf("list missing the user's subscription: %s", list)
			}
			if strings.Contains(list, "*Economist*") {
				t.Errorf("list contains another user's subscription: %s", list)
			}
		})
	}
}

func TestNotify(t *testing.T) {
	store := &fakeStore{matches: []string{testMessage().Sender.String(), "34999999999@c.us"}}
	svc, jwt := newTestService(store)
	sender := &fakeSender{}

	err := svc.Notify(context.Background(), sender, "nacional", "ElMundo_hoy.pdf")
	if err != nil {
		t.Fatalf("Notify failed: %v", err)
	}

	if len(sender.sentTexts) != 2 {
		t.Fatalf("expected 2 notifications, got %d", len(sender.sentTexts))
	}

	msg := sender.sentTexts[0]
	if msg.to.String() != testMessage().Sender.String() {
		t.Errorf("unexpected recipient: %s", msg.to)
	}
	if !strings.Contains(msg.text, "Documento:*ElMundo_hoy.pdf*") {
		t.Errorf("missing document name: %s", msg.text)
	}
	if !strings.Contains(msg.text, "https://files.example.com:443/nacional_folder/ElMundo_hoy.pdf?access_token=sub-token") {
		t.Errorf("missing link: %s", msg.text)
	}
	if jwt.expiry != subscriptionTokenTTL {
		t.Errorf("expected 48h token, got %v", jwt.expiry)
	}
}

func TestNotifyUnknownCategory(t *testing.T) {
	store := &fakeStore{}
	svc, _ := newTestService(store)
	if err := svc.Notify(context.Background(), &fakeSender{}, "unknown", "f.pdf"); err == nil {
		t.Fatal("expected error for unknown category")
	}
}

func TestNotifySkipsInvalidJIDs(t *testing.T) {
	store := &fakeStore{matches: []string{"not-a-jid", testMessage().Sender.String()}}
	svc, _ := newTestService(store)
	sender := &fakeSender{}

	err := svc.Notify(context.Background(), sender, "nacional", "file.pdf")
	if err == nil {
		t.Fatal("expected partial failure error")
	}
	if len(sender.sentTexts) != 1 {
		t.Fatalf("expected 1 successful notification, got %d", len(sender.sentTexts))
	}
}
