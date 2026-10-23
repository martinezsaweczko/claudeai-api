package electricity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"go.opentelemetry.io/otel/codes"
)

const (
	// indicatorID is the ESIOS indicator for the electricity price (PVPC)
	indicatorID = "1001"
	// geoID is the ESIOS geo zone for the Iberian Peninsula
	geoID = "8741"
	// apiBaseURL is the ESIOS (REE) API base URL
	apiBaseURL = "https://api.esios.ree.es/indicators/"
)

// esiosResponse is the JSON response of the ESIOS API
type esiosResponse struct {
	Indicator struct {
		Values []struct {
			Value    float64   `json:"value"`
			Datetime time.Time `json:"datetime"`
		} `json:"values"`
	} `json:"indicator"`
}

// fetch retrieves the hourly electricity prices for the given date range
func (s *Service) fetch(ctx context.Context, startDate, endDate time.Time) ([]PricePoint, error) {
	ctx, span := s.tracer.Start(ctx, "electricity.Fetch")
	defer span.End()

	// The Node version encodes colons as %3A; url.Values does that for us
	params := url.Values{
		"geo_ids[]":  {geoID},
		"start_date": {startDate.UTC().Format(time.RFC3339)},
		"end_date":   {endDate.UTC().Format(time.RFC3339)},
	}

	reqURL := s.baseURL + indicatorID + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		err = fmt.Errorf("failed to build ESIOS request: %w", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	req.Header.Set("x-api-key", s.apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		err = fmt.Errorf("ESIOS request failed: %w", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("ESIOS API returned status %d", resp.StatusCode)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	var parsed esiosResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		err = fmt.Errorf("failed to parse ESIOS response: %w", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	points := make([]PricePoint, 0, len(parsed.Indicator.Values))
	for _, v := range parsed.Indicator.Values {
		points = append(points, PricePoint{Datetime: v.Datetime, Value: v.Value})
	}

	s.log.Info("Fetched data points from ESIOS", "count", len(points))
	return points, nil
}

// splitByDate splits points into those of today and those of tomorrow (UTC dates)
func splitByDate(points []PricePoint, todayStr, tomorrowStr string) (today, tomorrow []PricePoint) {
	for _, p := range points {
		day := p.Datetime.UTC().Format("2006-01-02")
		switch day {
		case todayStr:
			today = append(today, p)
		case tomorrowStr:
			tomorrow = append(tomorrow, p)
		}
	}
	return today, tomorrow
}
