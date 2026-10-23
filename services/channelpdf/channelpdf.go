// Package channelpdf implements automatic PDF download from a configured
// WhatsApp group/channel. When a document message arrives from that JID, the
// attachment is saved to the kiosk folder of the configured category and
// matching subscribers are notified.
package channelpdf

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/martinezsaweczko/whatsappBot-golang/o11"
	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
	"go.mau.fi/whatsmeow/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// MediaDownloader downloads the document attached to an incoming message.
type MediaDownloader interface {
	DownloadMedia(ctx context.Context, msg whatsapp.IncomingMessage) ([]byte, error)
}

// Notifier notifies subscribers that a new file is available.
type Notifier interface {
	Notify(ctx context.Context, sender whatsapp.Sender, category, filename string) error
}

// Service watches a configured channel/group for PDF documents.
type Service struct {
	channelJID string
	category   string
	folders    map[string]string // category name -> directory
	downloader MediaDownloader
	notifier   Notifier
	log        *slog.Logger
	tracer     trace.Tracer

	downloadsTotal metric.Int64Counter
	savedTotal     metric.Int64Counter
	notifyTotal    metric.Int64Counter
}

// New creates the channel PDF service.
func New(channelJID, category string, folders map[string]string, downloader MediaDownloader, notifier Notifier, log *slog.Logger, tp trace.TracerProvider, mp metric.MeterProvider) (*Service, error) {
	dir, ok := folders[category]
	if !ok {
		return nil, fmt.Errorf("unknown pdf category %q", category)
	}
	if dir == "" {
		return nil, fmt.Errorf("pdf category %q has no directory configured", category)
	}
	if channelJID == "" {
		return nil, fmt.Errorf("pdf channel JID cannot be empty")
	}
	if jid, err := types.ParseJID(channelJID); err != nil || jid.User == "" || jid.Server == "" {
		return nil, fmt.Errorf("invalid pdf channel JID %q", channelJID)
	}

	downloadsTotal, err := o11.NewBusinessCounter(mp, "channelpdf_downloads_total", "PDF downloads from the monitored channel")
	if err != nil {
		return nil, fmt.Errorf("failed to create downloads counter: %w", err)
	}
	savedTotal, err := o11.NewBusinessCounter(mp, "channelpdf_saved_total", "PDFs saved to the kiosk folder")
	if err != nil {
		return nil, fmt.Errorf("failed to create saved counter: %w", err)
	}
	notifyTotal, err := o11.NewBusinessCounter(mp, "channelpdf_notify_total", "Subscriber notifications triggered by channel PDFs")
	if err != nil {
		return nil, fmt.Errorf("failed to create notify counter: %w", err)
	}

	return &Service{
		channelJID:     channelJID,
		category:       category,
		folders:        folders,
		downloader:     downloader,
		notifier:       notifier,
		log:            log,
		tracer:         tp.Tracer("services/channelpdf"),
		downloadsTotal: downloadsTotal,
		savedTotal:     savedTotal,
		notifyTotal:    notifyTotal,
	}, nil
}

// Handler returns the router hook that processes incoming channel documents.
func (s *Service) Handler() whatsapp.ChannelMediaHandler {
	return s.handle
}

func (s *Service) handle(ctx context.Context, sender whatsapp.Sender, msg whatsapp.IncomingMessage) error {
	ctx, span := s.tracer.Start(ctx, "channelpdf.Handle", trace.WithAttributes(
		attribute.String("chat", msg.Chat.String()),
		attribute.String("filename", msg.DocumentFileName),
		attribute.String("category", s.category),
	))
	defer span.End()

	if msg.Chat.String() != s.channelJID {
		return nil
	}
	if !msg.HasDocument {
		return nil
	}

	filename := strings.TrimSpace(msg.DocumentFileName)
	if filename == "" {
		s.log.Warn("Channel document has no filename, skipping", "chat", msg.Chat.String())
		return nil
	}

	log := s.log.With("chat", msg.Chat.String(), "filename", filename, "category", s.category)
	log.Info("Processing channel PDF")

	data, err := s.downloader.DownloadMedia(ctx, msg)
	if err != nil {
		span.RecordError(err)
		s.downloadsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "error")))
		return fmt.Errorf("failed to download channel PDF: %w", err)
	}
	s.downloadsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "ok")))

	destPath := filepath.Join(s.folders[s.category], filename)
	if _, err := os.Stat(destPath); err == nil {
		log.Info("Channel PDF already exists, skipping save and notification")
		s.savedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "exists")))
		return nil
	} else if !os.IsNotExist(err) {
		span.RecordError(err)
		s.savedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "error")))
		return fmt.Errorf("failed to check existing file: %w", err)
	}

	if err := os.WriteFile(destPath, data, 0o644); err != nil {
		span.RecordError(err)
		s.savedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "error")))
		return fmt.Errorf("failed to save channel PDF: %w", err)
	}
	s.savedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "new")))
	log.Info("Channel PDF saved", "path", destPath)

	if err := s.notifier.Notify(ctx, sender, s.category, filename); err != nil {
		span.RecordError(err)
		s.notifyTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "error")))
		return fmt.Errorf("failed to notify subscribers: %w", err)
	}
	s.notifyTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "ok")))

	return nil
}
