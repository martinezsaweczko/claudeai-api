package whatsapp

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/martinezsaweczko/whatsappBot-golang/o11"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/protobuf/proto"
)

func newTestRouter(t *testing.T, botJIDs []string) *Router {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	metrics, err := o11.NewCommandMetrics(noop.NewMeterProvider())
	if err != nil {
		t.Fatalf("failed to create metrics: %v", err)
	}
	return NewRouter(log, metrics, tracenoop.NewTracerProvider(), botJIDs, 5*time.Second)
}

func messageEvent(body string, mentioned ...string) *events.Message {
	msg := &waE2E.Message{}
	if mentioned == nil {
		msg.Conversation = proto.String(body)
	} else {
		msg.ExtendedTextMessage = &waE2E.ExtendedTextMessage{
			Text: proto.String(body),
			ContextInfo: &waE2E.ContextInfo{
				MentionedJID: mentioned,
			},
		}
	}
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:   types.NewJID("12345", types.GroupServer),
				Sender: types.NewJID("34645568517", types.DefaultUserServer),
			},
			ID:       "MSGID1",
			PushName: "David",
		},
		Message:    msg,
		RawMessage: msg,
	}
}

// waitFor polls a condition until it holds or the timeout expires
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func TestFirstMatchWins(t *testing.T) {
	router := newTestRouter(t, nil)

	var first, second atomic.Int32
	router.Register("first", regexp.MustCompile(`^hola`), func(_ context.Context, _ Sender, _ IncomingMessage) error {
		first.Add(1)
		return nil
	})
	router.Register("second", regexp.MustCompile(`^hola.*`), func(_ context.Context, _ Sender, _ IncomingMessage) error {
		second.Add(1)
		return nil
	})

	router.dispatch(messageEvent("hola que tal"))
	waitFor(t, func() bool { return first.Load() == 1 })

	if second.Load() != 0 {
		t.Error("second handler should not run when first matches")
	}
}

func TestNoMatchNoMentionNoDispatch(t *testing.T) {
	router := newTestRouter(t, []string{"57905285959776@lid"})

	var calls atomic.Int32
	router.Register("cmd", regexp.MustCompile(`^nacional`), func(_ context.Context, _ Sender, _ IncomingMessage) error {
		calls.Add(1)
		return nil
	})
	router.SetFallback(func(_ context.Context, _ Sender, _ IncomingMessage) error {
		calls.Add(1)
		return nil
	})

	router.dispatch(messageEvent("mensaje cualquiera sin mencion"))

	// Give the dispatcher a chance to (incorrectly) fire
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 0 {
		t.Error("no handler should run for unmatched message without mention")
	}
}

func TestFallbackOnMention(t *testing.T) {
	router := newTestRouter(t, []string{"57905285959776@lid", "34936674253@c.us"})

	var fallbackCalls atomic.Int32
	router.SetFallback(func(_ context.Context, _ Sender, _ IncomingMessage) error {
		fallbackCalls.Add(1)
		return nil
	})

	// Mention via @lid
	router.dispatch(messageEvent("@bot como estas", "57905285959776@lid"))
	waitFor(t, func() bool { return fallbackCalls.Load() == 1 })

	// Mention via @c.us
	router.dispatch(messageEvent("@bot dime algo", "34936674253@c.us"))
	waitFor(t, func() bool { return fallbackCalls.Load() == 2 })
}

func TestOwnMessagesIgnored(t *testing.T) {
	router := newTestRouter(t, nil)

	var calls atomic.Int32
	router.Register("cmd", regexp.MustCompile(`.*`), func(_ context.Context, _ Sender, _ IncomingMessage) error {
		calls.Add(1)
		return nil
	})

	evt := messageEvent("hola")
	evt.Info.IsFromMe = true
	router.dispatch(evt)

	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 0 {
		t.Error("messages sent by the bot itself must be ignored")
	}
}

func TestPanicRecovery(t *testing.T) {
	router := newTestRouter(t, nil)

	var survived atomic.Int32
	router.Register("panic", regexp.MustCompile(`^boom`), func(_ context.Context, _ Sender, _ IncomingMessage) error {
		panic("handler exploded")
	})
	router.Register("survivor", regexp.MustCompile(`^ok`), func(_ context.Context, _ Sender, _ IncomingMessage) error {
		survived.Add(1)
		return nil
	})

	router.dispatch(messageEvent("boom"))
	router.dispatch(messageEvent("ok"))

	waitFor(t, func() bool { return survived.Load() == 1 })
}

func TestHandlerErrorRecorded(t *testing.T) {
	router := newTestRouter(t, nil)

	done := make(chan struct{})
	router.Register("fail", regexp.MustCompile(`^fail`), func(_ context.Context, _ Sender, _ IncomingMessage) error {
		close(done)
		return errors.New("handler error")
	})

	router.dispatch(messageEvent("fail"))
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not run")
	}

	// Router must remain usable
	var ok atomic.Int32
	router.Register("ok", regexp.MustCompile(`^ok`), func(_ context.Context, _ Sender, _ IncomingMessage) error {
		ok.Add(1)
		return nil
	})
	router.dispatch(messageEvent("ok"))
	waitFor(t, func() bool { return ok.Load() == 1 })
}

func TestExtractTextAndMentions(t *testing.T) {
	// Conversation
	m := &waE2E.Message{Conversation: proto.String("hola")}
	if got := extractText(m); got != "hola" {
		t.Errorf("extractText conversation = %q", got)
	}

	// Extended text with mentions
	m2 := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String("@bot hola"),
			ContextInfo: &waE2E.ContextInfo{MentionedJID: []string{"123@lid"}},
		},
	}
	if got := extractText(m2); got != "@bot hola" {
		t.Errorf("extractText extended = %q", got)
	}
	if got := extractMentions(m2); len(got) != 1 || got[0] != "123@lid" {
		t.Errorf("extractMentions = %v", got)
	}

	// Nil safety
	if got := extractText(nil); got != "" {
		t.Errorf("extractText nil = %q", got)
	}
	if got := extractMentions(nil); got != nil {
		t.Errorf("extractMentions nil = %v", got)
	}
}

func TestNewIncomingMessageCopiesSenderAddresses(t *testing.T) {
	lid := types.NewJID("111222333444555", types.HiddenUserServer)
	phone := types.NewJID("34600111222", types.DefaultUserServer)

	tests := []struct {
		name      string
		sender    types.JID
		senderAlt types.JID
	}{
		{"LID sender with phone number alternate", lid, phone},
		{"phone number sender with LID alternate", phone, lid},
		{"no alternate address", phone, types.EmptyJID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evt := &events.Message{
				Info: types.MessageInfo{
					MessageSource: types.MessageSource{
						Chat:      types.NewJID("123456", types.GroupServer),
						Sender:    tt.sender,
						SenderAlt: tt.senderAlt,
						IsGroup:   true,
					},
				},
				Message: &waE2E.Message{Conversation: proto.String("list_subs")},
			}

			msg := NewIncomingMessage(evt)
			if msg.Sender != tt.sender {
				t.Errorf("Sender = %s, want %s", msg.Sender, tt.sender)
			}
			if msg.SenderAlt != tt.senderAlt {
				t.Errorf("SenderAlt = %s, want %s", msg.SenderAlt, tt.senderAlt)
			}
		})
	}
}

func TestMediaTypeFor(t *testing.T) {
	cases := map[string]string{
		"image/jpeg":      "image",
		"audio/mpeg":      "audio",
		"video/mp4":       "video",
		"application/pdf": "document",
	}
	for mime := range cases {
		_ = mediaTypeFor(mime) // smoke test: no panic
	}
}

func documentMessageEvent(chatJID types.JID, filename, mimeType string) *events.Message {
	msg := &waE2E.Message{
		DocumentMessage: &waE2E.DocumentMessage{
			Mimetype: proto.String(mimeType),
			FileName: proto.String(filename),
		},
	}
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:    chatJID,
				Sender:  types.NewJID("34645568517", types.DefaultUserServer),
				IsGroup: chatJID.Server == types.GroupServer,
			},
			ID:       "MSGID1",
			PushName: "David",
		},
		Message:    msg,
		RawMessage: msg,
	}
}

func TestChannelMediaHandler(t *testing.T) {
	channelJID := types.NewJID("120363420531996267", types.GroupServer)
	router := newTestRouter(t, nil)

	var calls atomic.Int32
	if err := router.RegisterChannelMediaHandler(channelJID.String(), func(_ context.Context, _ Sender, _ IncomingMessage) error {
		calls.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("failed to register channel media handler: %v", err)
	}

	router.dispatch(documentMessageEvent(channelJID, "paper.pdf", "application/pdf"))
	waitFor(t, func() bool { return calls.Load() == 1 })
}

func TestChannelMediaRunsWithoutBody(t *testing.T) {
	channelJID := types.NewJID("120363420531996267", types.GroupServer)
	router := newTestRouter(t, nil)

	var channelCalls, commandCalls atomic.Int32
	if err := router.RegisterChannelMediaHandler(channelJID.String(), func(_ context.Context, _ Sender, _ IncomingMessage) error {
		channelCalls.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("failed to register channel media handler: %v", err)
	}
	router.Register("cmd", regexp.MustCompile(`.*`), func(_ context.Context, _ Sender, _ IncomingMessage) error {
		commandCalls.Add(1)
		return nil
	})

	// Document message with no text body must still trigger the channel handler.
	router.dispatch(documentMessageEvent(channelJID, "paper.pdf", "application/pdf"))
	waitFor(t, func() bool { return channelCalls.Load() == 1 })

	if commandCalls.Load() != 0 {
		t.Error("regex command should not run for channel media messages")
	}
}

func TestChannelMediaIgnoredForOtherJIDs(t *testing.T) {
	channelJID := types.NewJID("120363420531996267", types.GroupServer)
	otherJID := types.NewJID("99999", types.GroupServer)
	router := newTestRouter(t, nil)

	var calls atomic.Int32
	if err := router.RegisterChannelMediaHandler(channelJID.String(), func(_ context.Context, _ Sender, _ IncomingMessage) error {
		calls.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("failed to register channel media handler: %v", err)
	}

	router.dispatch(documentMessageEvent(otherJID, "paper.pdf", "application/pdf"))

	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 0 {
		t.Error("channel media handler should not run for a different JID")
	}
}

func TestChannelMediaRequiresDocument(t *testing.T) {
	channelJID := types.NewJID("120363420531996267", types.GroupServer)
	router := newTestRouter(t, nil)

	var calls atomic.Int32
	if err := router.RegisterChannelMediaHandler(channelJID.String(), func(_ context.Context, _ Sender, _ IncomingMessage) error {
		calls.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("failed to register channel media handler: %v", err)
	}

	router.dispatch(messageEvent("text without document"))

	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 0 {
		t.Error("channel media handler should not run for messages without a document")
	}
}

func TestChannelMediaInvalidJID(t *testing.T) {
	router := newTestRouter(t, nil)
	err := router.RegisterChannelMediaHandler("not-a-jid", func(_ context.Context, _ Sender, _ IncomingMessage) error {
		return nil
	})
	if err == nil {
		t.Error("expected error when registering an invalid channel media JID")
	}
}

func TestMatchReturnsFirstCommand(t *testing.T) {
	router := newTestRouter(t, nil)
	router.Register("electricidad", regexp.MustCompile(`^(?i)elec(tricidad)?`), func(_ context.Context, _ Sender, _ IncomingMessage) error { return nil })

	name, err := router.Match("electricidad")
	if err != nil || name != "electricidad" {
		t.Fatalf("expected electricidad, got %q %v", name, err)
	}

	_, err = router.Match("unknown")
	if err == nil {
		t.Fatal("expected error for unmatched command")
	}
}

func TestExecuteCommandSynthetic(t *testing.T) {
	router := newTestRouter(t, nil)

	var got IncomingMessage
	router.Register("electricidad", regexp.MustCompile(`^(?i)elec(tricidad)?`), func(_ context.Context, _ Sender, msg IncomingMessage) error {
		got = msg
		return nil
	})

	sender := &fakeSender{}
	router.SetSender(sender)

	chat := types.NewJID("123456789", types.GroupServer)
	senderJID := types.NewJID("bot", types.DefaultUserServer)
	if err := router.ExecuteCommand(context.Background(), "electricidad", chat, senderJID); err != nil {
		t.Fatalf("ExecuteCommand failed: %v", err)
	}

	if got.Body != "electricidad" {
		t.Fatalf("unexpected body: %q", got.Body)
	}
	if got.Chat.String() != chat.String() {
		t.Fatalf("unexpected chat: %s", got.Chat.String())
	}
	if got.Sender.String() != senderJID.String() {
		t.Fatalf("unexpected sender: %s", got.Sender.String())
	}
	if !got.Synthetic {
		t.Fatal("expected synthetic message")
	}
	if !got.IsGroup {
		t.Fatal("expected group chat")
	}
}

func TestExecuteCommandNoMatch(t *testing.T) {
	router := newTestRouter(t, nil)
	router.SetSender(&fakeSender{})

	err := router.ExecuteCommand(context.Background(), "unknown", types.NewJID("123@g.us", types.GroupServer), types.EmptyJID)
	if err == nil {
		t.Fatal("expected error for unmatched command")
	}
}

func TestExecuteCommandRequiresSender(t *testing.T) {
	router := newTestRouter(t, nil)
	router.Register("cmd", regexp.MustCompile(`.*`), func(_ context.Context, _ Sender, _ IncomingMessage) error { return nil })

	err := router.ExecuteCommand(context.Background(), "cmd", types.NewJID("123@g.us", types.GroupServer), types.EmptyJID)
	if err == nil {
		t.Fatal("expected error when sender not set")
	}
}

// Scheduled runs must produce the same whatsapp.command.<name> span as real
// messages, as a child of the caller's span, and mark it failed when the
// handler returns an error.
func TestExecuteCommandTracing(t *testing.T) {
	handlerErr := errors.New("handler error")

	tests := []struct {
		name       string
		handlerErr error
		wantStatus codes.Code
		wantEvents int
	}{
		{name: "handler succeeds", wantStatus: codes.Unset},
		{name: "handler fails", handlerErr: handlerErr, wantStatus: codes.Error, wantEvents: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
			metrics, err := o11.NewCommandMetrics(noop.NewMeterProvider())
			if err != nil {
				t.Fatalf("failed to create metrics: %v", err)
			}
			router := NewRouter(log, metrics, tp, nil, 5*time.Second)
			router.SetSender(&fakeSender{})

			var handlerSpan trace.SpanContext
			router.Register("electricidad", regexp.MustCompile(`^(?i)elec(tricidad)?`), func(ctx context.Context, _ Sender, _ IncomingMessage) error {
				handlerSpan = trace.SpanContextFromContext(ctx)
				return tt.handlerErr
			})

			chat := types.NewJID("123456789", types.GroupServer)
			senderJID := types.NewJID("bot", types.DefaultUserServer)
			parentCtx, parent := tp.Tracer("test").Start(context.Background(), "scheduler.RunJob")
			err = router.ExecuteCommand(parentCtx, "electricidad", chat, senderJID)
			parent.End()

			if !errors.Is(err, tt.handlerErr) {
				t.Fatalf("ExecuteCommand error = %v, want %v", err, tt.handlerErr)
			}

			var span sdktrace.ReadOnlySpan
			for _, s := range recorder.Ended() {
				if s.Name() == "whatsapp.command.electricidad" {
					span = s
				}
			}
			if span == nil {
				t.Fatalf("no whatsapp.command.electricidad span among %d ended span(s)", len(recorder.Ended()))
			}
			if span.Parent().SpanID() != parent.SpanContext().SpanID() {
				t.Error("command span should be a child of the caller's span")
			}
			if handlerSpan.SpanID() != span.SpanContext().SpanID() {
				t.Error("handler context should carry the command span")
			}
			if got := span.Status().Code; got != tt.wantStatus {
				t.Errorf("span status = %v, want %v", got, tt.wantStatus)
			}
			if got := len(span.Events()); got != tt.wantEvents {
				t.Errorf("span has %d event(s), want %d", got, tt.wantEvents)
			}

			want := map[string]string{
				"command": "electricidad",
				"chat":    chat.String(),
				"sender":  senderJID.String(),
			}
			for _, kv := range span.Attributes() {
				if v, ok := want[string(kv.Key)]; ok && kv.Value.AsString() == v {
					delete(want, string(kv.Key))
				}
			}
			if len(want) != 0 {
				t.Errorf("command span is missing attributes %v", want)
			}
		})
	}
}

// fakeSender implements Sender for tests.
type fakeSender struct{}

func (f *fakeSender) ReplyText(ctx context.Context, msg IncomingMessage, text string) error {
	return nil
}
func (f *fakeSender) SendText(ctx context.Context, to types.JID, text string) error { return nil }
func (f *fakeSender) ReplyMedia(ctx context.Context, msg IncomingMessage, data []byte, mimeType, filename string) error {
	return nil
}
func (f *fakeSender) SendMedia(ctx context.Context, to types.JID, data []byte, mimeType, filename string) error {
	return nil
}
func (f *fakeSender) DeleteMessage(ctx context.Context, msg IncomingMessage) error { return nil }

func TestExtractDocument(t *testing.T) {
	// Document message
	m := &waE2E.Message{
		DocumentMessage: &waE2E.DocumentMessage{
			Mimetype: proto.String("application/pdf"),
			FileName: proto.String("paper.pdf"),
		},
	}
	hasDoc, mime, filename := extractDocument(m)
	if !hasDoc || mime != "application/pdf" || filename != "paper.pdf" {
		t.Errorf("extractDocument = (%v, %q, %q), want (true, application/pdf, paper.pdf)", hasDoc, mime, filename)
	}

	// Non-document message
	hasDoc, mime, filename = extractDocument(&waE2E.Message{Conversation: proto.String("hi")})
	if hasDoc || mime != "" || filename != "" {
		t.Errorf("extractDocument non-doc = (%v, %q, %q), want (false, , )", hasDoc, mime, filename)
	}

	// Nil safety
	hasDoc, mime, filename = extractDocument(nil)
	if hasDoc || mime != "" || filename != "" {
		t.Errorf("extractDocument nil = (%v, %q, %q), want (false, , )", hasDoc, mime, filename)
	}
}

func TestBotMentionJIDs(t *testing.T) {
	tests := []struct {
		name      string
		botNumber string
		lidJID    string
		want      []string
	}{
		{
			name:      "number and lid",
			botNumber: "34936674253",
			lidJID:    "57905285959776@lid",
			want:      []string{"57905285959776@lid", "34936674253@s.whatsapp.net", "34936674253@c.us"},
		},
		{
			name:      "no lid configured",
			botNumber: "34936674253",
			want:      []string{"34936674253@s.whatsapp.net", "34936674253@c.us"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BotMentionJIDs(tt.botNumber, tt.lidJID)
			if !slices.Equal(got, tt.want) {
				t.Errorf("BotMentionJIDs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFallbackOnPhoneNumberMention(t *testing.T) {
	router := newTestRouter(t, BotMentionJIDs("34936674253", ""))

	var fallbackCalls atomic.Int32
	router.SetFallback(func(_ context.Context, _ Sender, _ IncomingMessage) error {
		fallbackCalls.Add(1)
		return nil
	})

	// whatsmeow reports phone-number mentions on the s.whatsapp.net server
	router.dispatch(messageEvent("@bot dime algo", "34936674253@s.whatsapp.net"))
	waitFor(t, func() bool { return fallbackCalls.Load() == 1 })
}
