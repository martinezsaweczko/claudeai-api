package electricity

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/martinezsaweczko/whatsappBot-golang/o11"
	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Service provides electricity price charts
type Service struct {
	client   *http.Client
	apiKey   string
	baseURL  string // Overridable for tests; defaults to the ESIOS API
	cacheDir string
	log      *slog.Logger
	tracer   trace.Tracer

	cacheTotal  metric.Int64Counter
	chartsTotal metric.Int64Counter
}

// New creates the electricity service
func New(client *http.Client, apiKey, cacheDir string, log *slog.Logger, tp trace.TracerProvider, mp metric.MeterProvider) (*Service, error) {
	cacheTotal, err := o11.NewBusinessCounter(mp, "electricity_cache_total", "Electricity price cache results (hit, miss, tomorrow_fetch)")
	if err != nil {
		return nil, fmt.Errorf("failed to create cache counter: %w", err)
	}
	chartsTotal, err := o11.NewBusinessCounter(mp, "charts_rendered_total", "Electricity charts rendered")
	if err != nil {
		return nil, fmt.Errorf("failed to create charts counter: %w", err)
	}

	return &Service{
		client:      client,
		apiKey:      apiKey,
		baseURL:     apiBaseURL,
		cacheDir:    cacheDir,
		log:         log,
		tracer:      tp.Tracer("services/electricity"),
		cacheTotal:  cacheTotal,
		chartsTotal: chartsTotal,
	}, nil
}

// Commands returns the WhatsApp commands of the electricity service
func (s *Service) Commands() []whatsapp.Command {
	return []whatsapp.Command{
		{Name: "electricidad", Pattern: regexp.MustCompile(`^(?i)elec(tricidad)?`), Handler: s.electricityHandler()},
	}
}

// isNextDayFetchWindow reports whether we are in the 19:00-23:00 Madrid window
// in which tomorrow's prices are published
func isNextDayFetchWindow(now time.Time) bool {
	hour := now.In(madridLocation()).Hour()
	return hour >= 19 && hour < 23
}

// dayRange returns the fetch range: today 00:00 UTC to tomorrow 23:59:59 UTC
func dayRange(todayStr, tomorrowStr string) (time.Time, time.Time, error) {
	start, err := time.Parse("2006-01-02", todayStr)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err := time.Parse("2006-01-02", tomorrowStr)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end = end.Add(24*time.Hour - time.Second)
	return start, end, nil
}

// buildChartData implements the cache state machine and returns the chart input
func (s *Service) buildChartData(ctx context.Context, todayStr, tomorrowStr string) (*chartData, error) {
	ctx, span := s.tracer.Start(ctx, "electricity.BuildChartData")
	defer span.End()

	now := time.Now()
	inWindow := isNextDayFetchWindow(now)

	// Branch 1: today's cache exists
	if s.cacheExists(todayStr) {
		s.log.Info("Today's JSON found, loading from file")
		todayCache, err := s.loadCache(todayStr)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, err
		}
		s.cacheTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "hit")))

		data := &chartData{
			TodayDate: todayStr,
			Points:    todayCache.toPoints(),
		}

		// Fetch tomorrow's data when in the window and not attempted yet
		if inWindow && !todayCache.tomorrowFetched() {
			s.log.Info("Next-day fetch window detected and tomorrow not yet fetched, querying API")
			start, end, err := dayRange(todayStr, tomorrowStr)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
				return nil, err
			}
			raw, err := s.fetch(ctx, start, end)
			if err != nil {
				// Not fatal: today's chart is still served
				span.RecordError(err)
				s.log.Error("Failed to fetch tomorrow data", "error", err)
			} else if len(raw) > 0 {
				_, tomorrowData := splitByDate(raw, todayStr, tomorrowStr)
				if len(tomorrowData) > 0 {
					s.log.Info("Tomorrow's data found in API response, saving and merging")
					if _, err := s.saveCache(tomorrowStr, tomorrowData); err != nil {
						span.RecordError(err)
						s.log.Error("Failed to save tomorrow cache", "error", err)
					}
					s.updateTomorrowMetadata(todayStr, true)
					s.cacheTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "tomorrow_fetch")))
					data.Points = append(data.Points, tomorrowData...)
					data.TomorrowDate = tomorrowStr
					data.IsDualDay = true
				} else {
					s.log.Info("Tomorrow's data not yet available in API, will retry next window")
				}
			}
		} else if todayCache.tomorrowFetched() {
			// Merge with previously fetched tomorrow data if available
			if s.cacheExists(tomorrowStr) {
				tomorrowCache, err := s.loadCache(tomorrowStr)
				if err == nil && len(tomorrowCache.Data) > 0 {
					s.log.Info("Merging with previously fetched tomorrow data")
					data.Points = append(data.Points, tomorrowCache.toPoints()...)
					data.TomorrowDate = tomorrowStr
					data.IsDualDay = true
				}
			}
		}

		return data, nil
	}

	// Branch 2: no cache, fetch from API
	s.log.Info("Today's JSON not found, fetching from API")
	s.cacheTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "miss")))

	start, end, err := dayRange(todayStr, tomorrowStr)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	raw, err := s.fetch(ctx, start, end)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	if len(raw) == 0 {
		err := fmt.Errorf("no data available from API")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	todayData, tomorrowData := splitByDate(raw, todayStr, tomorrowStr)
	if len(todayData) == 0 {
		err := fmt.Errorf("no data available for today")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	if _, err := s.saveCache(todayStr, todayData); err != nil {
		span.RecordError(err)
		s.log.Error("Failed to save today's cache", "error", err)
	}

	data := &chartData{
		TodayDate: todayStr,
		Points:    todayData,
	}

	if inWindow && len(tomorrowData) > 0 {
		s.log.Info("Tomorrow's data found in API response, saving and merging")
		if _, err := s.saveCache(tomorrowStr, tomorrowData); err != nil {
			span.RecordError(err)
			s.log.Error("Failed to save tomorrow cache", "error", err)
		}
		s.updateTomorrowMetadata(todayStr, true)
		s.cacheTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "tomorrow_fetch")))
		data.Points = append(data.Points, tomorrowData...)
		data.TomorrowDate = tomorrowStr
		data.IsDualDay = true
	} else {
		if inWindow {
			s.updateTomorrowMetadata(todayStr, false)
		}
	}

	return data, nil
}

// PriceChart builds the price chart for today (and tomorrow when available)
// and returns the PNG bytes plus the date label
func (s *Service) PriceChart(ctx context.Context) ([]byte, error) {
	ctx, span := s.tracer.Start(ctx, "electricity.PriceChart")
	defer span.End()

	now := time.Now().UTC()
	todayStr := now.Format("2006-01-02")
	tomorrowStr := now.Add(24 * time.Hour).Format("2006-01-02")

	data, err := s.buildChartData(ctx, todayStr, tomorrowStr)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	png, err := s.renderChart(data)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	// Persist alongside the cache (same name as the Node version) for debugging
	pngPath := s.cacheDir + "/kwh-" + todayStr + ".png"
	if err := os.WriteFile(pngPath, png, 0o644); err != nil {
		span.RecordError(err)
		s.log.Warn("Failed to persist chart PNG", "error", err)
	}

	s.chartsTotal.Add(ctx, 1)
	return png, nil
}

func (s *Service) electricityHandler() whatsapp.CommandHandler {
	return func(ctx context.Context, sender whatsapp.Sender, msg whatsapp.IncomingMessage) error {
		s.log.Info("Electricity price requested", "user", msg.PushName)

		png, err := s.PriceChart(ctx)
		if err != nil {
			s.log.Error("Failed to build price chart", "error", err)
			return sender.ReplyText(ctx, msg, "No he podido obtener el precio de la electricidad ahora mismo")
		}

		return sender.ReplyMedia(ctx, msg, png, "image/png", "kwh.png")
	}
}
