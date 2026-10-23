// Package subscription implements keyword subscriptions: users subscribe to
// text patterns and get notified with a download link when a matching file arrives.
package subscription

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/martinezsaweczko/whatsappBot-golang/model"
	"github.com/martinezsaweczko/whatsappBot-golang/o11"
	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
	"go.mau.fi/whatsmeow/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// subscriptionTokenTTL is how long the JWT in a subscription link is valid
const subscriptionTokenTTL = 48 * time.Hour

// Store is the consumer-defined interface for subscription persistence
type Store interface {
	SaveSubscription(ctx context.Context, subscriptionText, user string) error
	DeleteSubscription(ctx context.Context, users []string) error
	ReturnSubscriptions(ctx context.Context, users []string) ([]model.Subscription, error)
	MatchSubscriptions(ctx context.Context, file string) ([]string, error)
}

// LinkBuilder issues subscription JWT tokens (consumer-defined interface)
type LinkBuilder interface {
	GenerateSubscriptionToken(subject string, expiry time.Duration) (string, error)
}

// Service provides subscription operations
type Service struct {
	store       Store
	jwt         LinkBuilder
	urlServer   string
	urlPrefixes map[string]string // category name -> URL prefix ("nacional" -> "nacional_folder")
	log         *slog.Logger
	tracer      trace.Tracer
	eventsTotal metric.Int64Counter
	notifyTotal metric.Int64Counter
}

// New creates the subscription service
func New(store Store, jwt LinkBuilder, urlServer string, urlPrefixes map[string]string, log *slog.Logger, tp trace.TracerProvider, mp metric.MeterProvider) (*Service, error) {
	eventsTotal, err := o11.NewBusinessCounter(mp, "subscription_events_total", "Subscription events per action (create, delete, list)")
	if err != nil {
		return nil, fmt.Errorf("failed to create events counter: %w", err)
	}
	notifyTotal, err := o11.NewBusinessCounter(mp, "subscription_notifications_sent_total", "Subscription notifications sent")
	if err != nil {
		return nil, fmt.Errorf("failed to create notifications counter: %w", err)
	}

	return &Service{
		store:       store,
		jwt:         jwt,
		urlServer:   urlServer,
		urlPrefixes: urlPrefixes,
		log:         log,
		tracer:      tp.Tracer("services/subscription"),
		eventsTotal: eventsTotal,
		notifyTotal: notifyTotal,
	}, nil
}

// userIdentities returns the equivalent identity strings of the message author
// (the subscription owner), without duplicates. WhatsApp addresses the same
// user by phone number or by LID, a linked device adds a device part, and rows
// imported from the Node app use the legacy "c.us" server, so no single string
// identifies a user. The first element is the canonical key for new
// subscriptions: the phone-number JID when known, otherwise the LID.
func userIdentities(msg whatsapp.IncomingMessage) []string {
	var phones, others []string
	for _, jid := range []types.JID{msg.Sender, msg.SenderAlt} {
		if jid.User == "" || jid.Server == "" {
			continue
		}
		switch jid.Server {
		case types.DefaultUserServer, types.LegacyUserServer:
			phones = append(phones,
				types.NewJID(jid.User, types.DefaultUserServer).String(),
				types.NewJID(jid.User, types.LegacyUserServer).String())
		default:
			others = append(others, jid.ToNonAD().String())
		}
	}

	var identities []string
	for _, identity := range append(phones, others...) {
		if !slices.Contains(identities, identity) {
			identities = append(identities, identity)
		}
	}
	return identities
}

// Subscribe stores a new subscription for the message author, under their canonical identity
func (s *Service) Subscribe(ctx context.Context, msg whatsapp.IncomingMessage, text string) error {
	ctx, span := s.tracer.Start(ctx, "subscription.Subscribe")
	defer span.End()

	text = strings.TrimSpace(text)
	if text == "" {
		err := fmt.Errorf("la subscripción no puede estar vacía")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	users := userIdentities(msg)
	if len(users) == 0 {
		err := fmt.Errorf("no se ha podido identificar al remitente")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	user := users[0]
	s.log.Info("Processing subscription", "user", msg.PushName, "text", text)

	if err := s.store.SaveSubscription(ctx, text, user); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	s.eventsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("action", "create")))
	return nil
}

// Delete removes all subscriptions of the message author, under any of their identities
func (s *Service) Delete(ctx context.Context, msg whatsapp.IncomingMessage) error {
	ctx, span := s.tracer.Start(ctx, "subscription.Delete")
	defer span.End()

	users := userIdentities(msg)
	s.log.Info("Subscription delete request", "user", msg.PushName)

	if err := s.store.DeleteSubscription(ctx, users); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	s.eventsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("action", "delete")))
	return nil
}

// List returns the formatted list of subscriptions of the message author, under any of their identities
func (s *Service) List(ctx context.Context, msg whatsapp.IncomingMessage) (string, error) {
	ctx, span := s.tracer.Start(ctx, "subscription.List")
	defer span.End()

	users := userIdentities(msg)
	subs, err := s.store.ReturnSubscriptions(ctx, users)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	s.eventsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("action", "list")))

	if len(subs) == 0 {
		return "No tienes subscripciones activas", nil
	}

	var b strings.Builder
	b.WriteString("A continuacion tienes el detalle de tus subscripciones:\n")
	for _, sub := range subs {
		fmt.Fprintf(&b, "*%s*\n", sub.SubscriptionText)
	}
	return b.String(), nil
}

// Notify sends a download link to every user whose subscription matches the file name
func (s *Service) Notify(ctx context.Context, sender whatsapp.Sender, category, filename string) error {
	ctx, span := s.tracer.Start(ctx, "subscription.Notify", trace.WithAttributes(
		attribute.String("category", category),
		attribute.String("file", filename),
	))
	defer span.End()

	prefix, ok := s.urlPrefixes[category]
	if !ok {
		err := fmt.Errorf("unknown category: %s", category)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	users, err := s.store.MatchSubscriptions(ctx, filename)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	s.log.Info("Notifying subscribers", "file", filename, "category", category, "matches", len(users))

	var errs []string
	for _, user := range users {
		// Stored subscribers must be full JIDs (user@server)
		if !strings.Contains(user, "@") {
			s.log.Error("Invalid subscriber JID, skipping", "user", user)
			errs = append(errs, "invalid JID: "+user)
			continue
		}
		jid, err := types.ParseJID(user)
		if err != nil {
			s.log.Error("Invalid subscriber JID, skipping", "user", user, "error", err)
			errs = append(errs, err.Error())
			continue
		}

		token, err := s.jwt.GenerateSubscriptionToken("periodico", subscriptionTokenTTL)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}

		link := url.URL{
			Scheme:   "https",
			Host:     s.urlServer,
			Path:     "/" + prefix + "/" + filename,
			RawQuery: "access_token=" + token,
		}

		message := "Documento:*" + filename + "*\nAqui tienes el enlace de tu subscripción\n" +
			link.String() + "\nEl enlace estará disponible sólo una única vez\n"

		if err := sender.SendText(ctx, jid, message); err != nil {
			s.log.Error("Failed to notify subscriber", "user", user, "error", err)
			errs = append(errs, err.Error())
			continue
		}
		s.notifyTotal.Add(ctx, 1)
	}

	if len(errs) > 0 {
		err := fmt.Errorf("some notifications failed: %s", strings.Join(errs, "; "))
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}
