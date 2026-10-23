package whatsapp

import (
	"context"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestHandleEventDispatchesMessages(t *testing.T) {
	router := newTestRouter(t, nil)
	sender := &fakeSender{}
	router.SetSender(sender)

	var got atomic.Pointer[IncomingMessage]
	var gotSender atomic.Value
	router.Register("hola", regexp.MustCompile(`^hola`), func(_ context.Context, s Sender, msg IncomingMessage) error {
		gotSender.Store(s)
		got.Store(&msg)
		return nil
	})

	router.HandleEvent(messageEvent("hola que tal"))
	// Wait returns only once the handler goroutine has finished.
	router.Wait()

	msg := got.Load()
	if msg == nil {
		t.Fatal("the handler had not run when Wait returned")
	}
	if msg.Body != "hola que tal" || msg.PushName != "David" || msg.ID != "MSGID1" {
		t.Errorf("handler received %+v", *msg)
	}
	if gotSender.Load() != Sender(sender) {
		t.Error("the handler did not receive the configured sender")
	}
}

func TestWaitBlocksUntilInFlightHandlersFinish(t *testing.T) {
	router := newTestRouter(t, nil)

	started := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool
	router.Register("slow", regexp.MustCompile(`^slow`), func(context.Context, Sender, IncomingMessage) error {
		close(started)
		<-release
		finished.Store(true)
		return nil
	})

	router.HandleEvent(messageEvent("slow"))
	<-started

	waited := make(chan struct{})
	var finishedWhenWaitReturned atomic.Bool
	go func() {
		router.Wait()
		finishedWhenWaitReturned.Store(finished.Load())
		close(waited)
	}()

	close(release)
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait never returned")
	}
	if !finishedWhenWaitReturned.Load() {
		t.Error("Wait returned before the in-flight handler finished")
	}
}

func TestWaitWithoutHandlersReturnsImmediately(t *testing.T) {
	router := newTestRouter(t, nil)

	done := make(chan struct{})
	go func() {
		router.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait blocked with nothing in flight")
	}
}

func TestHandleEventConnectedRunsTheHook(t *testing.T) {
	router := newTestRouter(t, nil)

	// Without a hook the event is only logged.
	router.HandleEvent(&events.Connected{})

	called := make(chan context.Context, 1)
	router.SetConnectedHook(func(ctx context.Context) { called <- ctx })
	router.HandleEvent(&events.Connected{})

	select {
	case ctx := <-called:
		if ctx == nil {
			t.Fatal("the connected hook received a nil context")
		}
		if err := ctx.Err(); err != nil {
			t.Errorf("the connected hook received a finished context: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the connected hook was never called")
	}
}

// The connected hook runs in its own goroutine, so a slow hook (for example
// one sending the restart notification) does not block the event loop.
func TestHandleEventConnectedDoesNotBlockOnTheHook(t *testing.T) {
	router := newTestRouter(t, nil)

	release := make(chan struct{})
	hookDone := make(chan struct{})
	router.SetConnectedHook(func(context.Context) {
		<-release
		close(hookDone)
	})

	returned := make(chan struct{})
	go func() {
		router.HandleEvent(&events.Connected{})
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("HandleEvent blocked on the connected hook")
	}
	close(release)
	<-hookDone
}

func TestHandleEventLoggedOutWaitsForHandlersThenRunsTheHook(t *testing.T) {
	router := newTestRouter(t, nil)

	started := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool
	router.Register("slow", regexp.MustCompile(`^slow`), func(context.Context, Sender, IncomingMessage) error {
		close(started)
		<-release
		finished.Store(true)
		return nil
	})

	hookCalls := 0
	handlerFinishedBeforeHook := false
	router.SetLoggedOutHook(func() {
		hookCalls++
		handlerFinishedBeforeHook = finished.Load()
	})

	router.HandleEvent(messageEvent("slow"))
	<-started

	returned := make(chan struct{})
	go func() {
		router.HandleEvent(&events.LoggedOut{})
		close(returned)
	}()

	close(release)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the logged-out event never finished")
	}

	// The hook runs synchronously inside HandleEvent, so reading these after
	// it returned is race-free.
	if hookCalls != 1 {
		t.Fatalf("logged-out hook called %d times, want 1", hookCalls)
	}
	if !handlerFinishedBeforeHook {
		t.Error("the logged-out hook ran before the in-flight handler finished")
	}
}

func TestHandleEventIgnoresOtherEvents(t *testing.T) {
	router := newTestRouter(t, nil)

	var ran atomic.Int32
	router.Register("any", regexp.MustCompile(`.*`), func(context.Context, Sender, IncomingMessage) error {
		ran.Add(1)
		return nil
	})
	connected, loggedOut := 0, 0
	router.SetConnectedHook(func(context.Context) { connected++ })
	router.SetLoggedOutHook(func() { loggedOut++ })

	// Without a hook the logged-out event must not panic either.
	newTestRouter(t, nil).HandleEvent(&events.LoggedOut{})

	for _, evt := range []any{&events.Disconnected{}, &events.Receipt{}, "not an event", nil} {
		router.HandleEvent(evt)
	}
	router.Wait()

	if ran.Load() != 0 || connected != 0 || loggedOut != 0 {
		t.Errorf("unrelated events triggered work: handlers=%d connected=%d loggedOut=%d", ran.Load(), connected, loggedOut)
	}
}

func TestNewSyntheticMessage(t *testing.T) {
	bot := types.NewJID("34936674253", types.DefaultUserServer)

	tests := []struct {
		name      string
		chat      types.JID
		wantGroup bool
	}{
		{"group chat", types.NewJID("120363420531996267", types.GroupServer), true},
		{"direct chat", types.NewJID("34600111222", types.DefaultUserServer), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := NewSyntheticMessage(tt.chat, bot, "electricidad")

			if !msg.Synthetic {
				t.Error("a synthetic message must be flagged as such")
			}
			if msg.IsGroup != tt.wantGroup {
				t.Errorf("IsGroup = %v, want %v", msg.IsGroup, tt.wantGroup)
			}
			if msg.Chat != tt.chat || msg.Sender != bot || msg.Body != "electricidad" {
				t.Errorf("unexpected message: %+v", msg)
			}
			if msg.RawMessage != nil || msg.ID != "" {
				t.Error("a synthetic message has nothing to quote")
			}
		})
	}
}

func TestNormalizeJID(t *testing.T) {
	tests := []struct {
		name string
		in   types.JID
		want types.JID
	}{
		{"legacy user server is rewritten", types.NewJID("34600111222", types.LegacyUserServer), types.NewJID("34600111222", types.DefaultUserServer)},
		{"current user server is kept", types.NewJID("34600111222", types.DefaultUserServer), types.NewJID("34600111222", types.DefaultUserServer)},
		{"groups are kept", types.NewJID("120363420531996267", types.GroupServer), types.NewJID("120363420531996267", types.GroupServer)},
		{"lid addresses are kept", types.NewJID("123456789", types.HiddenUserServer), types.NewJID("123456789", types.HiddenUserServer)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeJID(tt.in); got != tt.want {
				t.Errorf("normalizeJID(%s) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

func TestQuoteContextPointsAtTheOriginalMessage(t *testing.T) {
	bot := types.NewJID("34936674253", types.DefaultUserServer)
	evt := messageEvent("hola")
	msg := NewIncomingMessage(evt)

	ctxInfo := newOfflineClient(t, bot).quoteContext(msg)

	if ctxInfo.GetStanzaID() != "MSGID1" {
		t.Errorf("stanza ID = %q, want the quoted message ID", ctxInfo.GetStanzaID())
	}
	if ctxInfo.GetParticipant() != "34645568517@s.whatsapp.net" {
		t.Errorf("participant = %q, want the original author", ctxInfo.GetParticipant())
	}
	if ctxInfo.GetQuotedMessage() != evt.Message {
		t.Error("the quoted message should be the original one")
	}
}

func TestOfflineClientIdentityAndState(t *testing.T) {
	bot := types.NewJID("34936674253", types.DefaultUserServer)

	paired := newOfflineClient(t, bot)
	if got := paired.OwnJID(); got != bot {
		t.Errorf("OwnJID = %s, want %s", got, bot)
	}
	if paired.IsConnected() {
		t.Error("a client that never connected must not report itself as connected")
	}

	unpaired := &Client{cli: whatsmeow.NewClient(&store.Device{}, nil)}
	if got := unpaired.OwnJID(); got != types.EmptyJID {
		t.Errorf("OwnJID without a session = %s, want the empty JID", got)
	}
}
