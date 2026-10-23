package whatsapp

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/martinezsaweczko/whatsappBot-golang/o11"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Sender is the interface consumed by command handlers to interact with WhatsApp.
// It is defined here (consumer side) and implemented by the whatsmeow adapter (Client).
type Sender interface {
	// ReplyText sends a text message quoting the original message
	ReplyText(ctx context.Context, msg IncomingMessage, text string) error
	// SendText sends a plain text message to a JID
	SendText(ctx context.Context, to types.JID, text string) error
	// ReplyMedia sends media (image, audio, document...) quoting the original message
	ReplyMedia(ctx context.Context, msg IncomingMessage, data []byte, mimeType, filename string) error
	// SendMedia sends media to a JID without quoting
	SendMedia(ctx context.Context, to types.JID, data []byte, mimeType, filename string) error
	// DeleteMessage deletes (revokes) a message
	DeleteMessage(ctx context.Context, msg IncomingMessage) error
}

// CommandHandler handles a matched incoming message
type CommandHandler func(ctx context.Context, s Sender, msg IncomingMessage) error

// ChannelMediaHandler handles document messages arriving from a configured
// channel/group JID (e.g. automatic PDF downloads).
type ChannelMediaHandler func(ctx context.Context, s Sender, msg IncomingMessage) error

// Command binds a regex pattern to a handler. First matching command wins.
type Command struct {
	Name    string // Used in logs and metrics
	Pattern *regexp.Regexp
	Handler CommandHandler
}

// Router dispatches incoming WhatsApp messages to registered commands
type Router struct {
	log         *slog.Logger
	metrics     *o11.CommandMetrics
	tracer      trace.Tracer
	sender      Sender
	commands    []Command
	fallback    CommandHandler
	botJIDs     []string // JID forms that count as a bot mention (see BotMentionJIDs)
	timeout     time.Duration
	wg          sync.WaitGroup
	onConnected func(ctx context.Context)
	onLoggedOut func()

	// channelMedia, when set, receives document messages from the configured JID
	// before regular command matching.
	channelMedia struct {
		jid     types.JID
		handler ChannelMediaHandler
	}
}

// NewRouter creates a command router
func NewRouter(log *slog.Logger, metrics *o11.CommandMetrics, tp trace.TracerProvider, botJIDs []string, timeout time.Duration) *Router {
	return &Router{
		log:     log,
		metrics: metrics,
		tracer:  tp.Tracer("whatsapp/router"),
		botJIDs: botJIDs,
		timeout: timeout,
	}
}

// SetSender injects the WhatsApp sender used by command handlers.
// It is set at wiring time (after creating the client) to break the
// client <-> router construction cycle.
func (r *Router) SetSender(s Sender) {
	r.sender = s
}

// Register adds a command to the router. Commands are evaluated in registration order.
func (r *Router) Register(name string, pattern *regexp.Regexp, handler CommandHandler) {
	r.commands = append(r.commands, Command{Name: name, Pattern: pattern, Handler: handler})
	r.log.Debug("Registered command", "name", name, "pattern", pattern.String())
}

// Match returns the name of the first command whose pattern matches the given body.
// It returns an error when no command matches.
func (r *Router) Match(body string) (string, error) {
	for _, cmd := range r.commands {
		if cmd.Pattern.MatchString(body) {
			return cmd.Name, nil
		}
	}
	return "", fmt.Errorf("no command matches %q", body)
}

// ExecuteCommand runs the first command whose pattern matches body against a
// synthetic IncomingMessage. It is used for scheduled/internal commands that do
// not originate from a real WhatsApp event. The caller provides the bot's own
// JID to use as the synthetic sender.
func (r *Router) ExecuteCommand(ctx context.Context, body string, chat, senderJID types.JID) error {
	if r.sender == nil {
		return fmt.Errorf("router sender not set")
	}

	var matched Command
	for _, cmd := range r.commands {
		if cmd.Pattern.MatchString(body) {
			matched = cmd
			break
		}
	}
	if matched.Handler == nil {
		return fmt.Errorf("no command matches %q", body)
	}

	msg := NewSyntheticMessage(chat, senderJID, body)

	// Same span as execute, so scheduled runs look like real messages in traces.
	// The handler still runs synchronously on the caller's context.
	ctx, span := r.tracer.Start(ctx, "whatsapp.command."+matched.Name,
		trace.WithAttributes(
			attribute.String("command", matched.Name),
			attribute.String("chat", msg.Chat.String()),
			attribute.String("sender", msg.Sender.String()),
		))
	defer span.End()

	if err := matched.Handler(ctx, r.sender, msg); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

// SetFallback sets the handler invoked when no command matches and the bot is mentioned
func (r *Router) SetFallback(handler CommandHandler) {
	r.fallback = handler
}

// RegisterChannelMediaHandler registers a handler for document messages sent to
// the configured channel/group JID. It runs before regex command matching and
// does not require a text body.
func (r *Router) RegisterChannelMediaHandler(jid string, handler ChannelMediaHandler) error {
	parsed, err := types.ParseJID(jid)
	if err != nil || parsed.User == "" || parsed.Server == "" {
		return fmt.Errorf("invalid channel media JID %q", jid)
	}
	r.channelMedia.jid = parsed
	r.channelMedia.handler = handler
	r.log.Debug("Registered channel media handler", "jid", jid)
	return nil
}

// SetConnectedHook sets the callback invoked when the client connects
func (r *Router) SetConnectedHook(hook func(ctx context.Context)) {
	r.onConnected = hook
}

// SetLoggedOutHook sets the callback invoked when the client is logged out
func (r *Router) SetLoggedOutHook(hook func()) {
	r.onLoggedOut = hook
}

// Wait blocks until all in-flight command handlers finish (used during shutdown)
func (r *Router) Wait() {
	r.wg.Wait()
}

// HandleEvent is the whatsmeow event handler entry point
func (r *Router) HandleEvent(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		r.dispatch(v)
	case *events.Connected:
		r.log.Info("WhatsApp client connected")
		if r.onConnected != nil {
			go r.onConnected(context.Background())
		}
	case *events.LoggedOut:
		r.log.Error("WhatsApp client logged out")
		r.wg.Wait()
		if r.onLoggedOut != nil {
			r.onLoggedOut()
		}
	}
}

// dispatch routes a message to the first matching command, or the fallback
func (r *Router) dispatch(evt *events.Message) {
	msg := NewIncomingMessage(evt)

	// Ignore messages sent by the bot itself
	if evt.Info.IsFromMe {
		return
	}

	// Channel media handler takes precedence for document messages from the
	// configured channel/group JID, even when there is no text body.
	if r.channelMedia.handler != nil && r.matchesChannelMedia(msg) {
		r.execute("channel_media", CommandHandler(r.channelMedia.handler), msg)
		return
	}

	if msg.Body == "" {
		return
	}

	for _, cmd := range r.commands {
		if cmd.Pattern.MatchString(msg.Body) {
			r.execute(cmd.Name, cmd.Handler, msg)
			return
		}
	}

	if r.fallback != nil && r.mentionsBot(msg) {
		r.execute("ai_fallback", r.fallback, msg)
	}
}

// matchesChannelMedia reports whether the message is a document message sent to
// the configured channel/group JID.
func (r *Router) matchesChannelMedia(msg IncomingMessage) bool {
	if !msg.HasDocument {
		return false
	}
	return msg.Chat.String() == r.channelMedia.jid.String()
}

// BotMentionJIDs returns the JID forms that count as a mention of the bot: the
// optional @lid JID plus the phone number on the current and legacy user servers
func BotMentionJIDs(botNumber, lidJID string) []string {
	jids := make([]string, 0, 3)
	if lidJID != "" {
		jids = append(jids, lidJID)
	}
	return append(jids,
		botNumber+"@"+types.DefaultUserServer,
		botNumber+"@"+types.LegacyUserServer,
	)
}

// mentionsBot reports whether the message mentions the bot in any of its known JID forms
func (r *Router) mentionsBot(msg IncomingMessage) bool {
	for _, mentioned := range msg.MentionedJIDs {
		for _, botJID := range r.botJIDs {
			if botJID != "" && mentioned == botJID {
				return true
			}
		}
	}
	return false
}

// execute runs a handler in its own goroutine with timeout, panic recovery,
// metrics and tracing
func (r *Router) execute(name string, handler CommandHandler, msg IncomingMessage) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			if rec := recover(); rec != nil {
				r.log.Error("Command handler panicked",
					"command", name, "panic", fmt.Sprint(rec), "chat", msg.Chat.String())
				r.recordMetrics(context.Background(), name, "panic", 0)
			}
		}()

		ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
		defer cancel()

		ctx, span := r.tracer.Start(ctx, "whatsapp.command."+name,
			trace.WithAttributes(
				attribute.String("command", name),
				attribute.String("chat", msg.Chat.String()),
				attribute.String("sender", msg.Sender.String()),
			))
		defer span.End()

		log := r.log.With("command", name, "push_name", msg.PushName, "chat", msg.Chat.String())
		log.Info("Executing command", "body", msg.Body)

		start := time.Now()
		err := handler(ctx, r.sender, msg)
		duration := time.Since(start).Seconds()

		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			log.Error("Command failed", "error", err)
			r.recordMetrics(ctx, name, "error", duration)
			return
		}

		r.recordMetrics(ctx, name, "ok", duration)
	}()
}

func (r *Router) recordMetrics(ctx context.Context, name, result string, duration float64) {
	attrs := metric.WithAttributes(
		attribute.String("command", name),
		attribute.String("result", result),
	)
	r.metrics.CommandsTotal.Add(ctx, 1, attrs)
	if duration > 0 {
		r.metrics.CommandDuration.Record(ctx, duration,
			metric.WithAttributes(attribute.String("command", name)))
	}
}
