package electricity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cacheFileName returns the cache file name for a date (YYYY-MM-DD)
func (s *Service) cacheFileName(dateStr string) string {
	return filepath.Join(s.cacheDir, "kwh-"+dateStr+".json")
}

// cacheExists reports whether the cache file for a date exists
func (s *Service) cacheExists(dateStr string) bool {
	_, err := os.Stat(s.cacheFileName(dateStr))
	return err == nil
}

// loadCache reads the cache file for a date
func (s *Service) loadCache(dateStr string) (*cachedDay, error) {
	data, err := os.ReadFile(s.cacheFileName(dateStr))
	if err != nil {
		return nil, fmt.Errorf("failed to read cache for %s: %w", dateStr, err)
	}

	var day cachedDay
	if err := json.Unmarshal(data, &day); err != nil {
		return nil, fmt.Errorf("failed to parse cache for %s: %w", dateStr, err)
	}
	return &day, nil
}

// saveCache writes the cache file for a date
func (s *Service) saveCache(dateStr string, points []PricePoint) (*cachedDay, error) {
	madrid := madridLocation()
	day := cachedDay{
		Date:        dateStr,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Data:        make([]cachedPoint, 0, len(points)),
	}

	for _, p := range points {
		local := p.Datetime.In(madrid)
		day.Data = append(day.Data, cachedPoint{
			Datetime: p.Datetime.Format(time.RFC3339),
			Hour:     fmt.Sprintf("%02d:00", local.Hour()),
			Price:    fmt.Sprintf("%.3f", p.Value/1000),
			RawPrice: p.Value,
		})
	}

	data, err := json.MarshalIndent(day, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to serialize cache: %w", err)
	}

	if err := os.WriteFile(s.cacheFileName(dateStr), data, 0o644); err != nil {
		return nil, fmt.Errorf("failed to write cache: %w", err)
	}

	s.log.Info("JSON data saved successfully", "file", s.cacheFileName(dateStr))
	return &day, nil
}

// updateTomorrowMetadata marks in today's cache whether tomorrow's data was fetched
func (s *Service) updateTomorrowMetadata(todayStr string, fetched bool) {
	day, err := s.loadCache(todayStr)
	if err != nil {
		s.log.Error("Failed to load cache for metadata update", "date", todayStr, "error", err)
		return
	}

	var marker string
	if fetched {
		marker = time.Now().UTC().Format(time.RFC3339)
	} else {
		marker = "failed"
	}
	day.TomorrowFetchedAt = &marker

	data, err := json.MarshalIndent(day, "", "  ")
	if err != nil {
		s.log.Error("Failed to serialize metadata update", "error", err)
		return
	}
	if err := os.WriteFile(s.cacheFileName(todayStr), data, 0o644); err != nil {
		s.log.Error("Failed to write metadata update", "error", err)
	}
}

// toPoints converts cached points back to price points
func (c *cachedDay) toPoints() []PricePoint {
	points := make([]PricePoint, 0, len(c.Data))
	for _, p := range c.Data {
		dt, err := time.Parse(time.RFC3339, p.Datetime)
		if err != nil {
			continue
		}
		points = append(points, PricePoint{Datetime: dt, Value: p.RawPrice})
	}
	return points
}

// tomorrowFetched reports whether tomorrow's data fetch was already attempted.
// Mirrors the Node behavior: any non-null marker (including "failed") blocks refetch.
func (c *cachedDay) tomorrowFetched() bool {
	return c.TomorrowFetchedAt != nil && *c.TomorrowFetchedAt != ""
}

// deleteOldFiles removes kwh-* cache files older than 2 days
func (s *Service) deleteOldFiles() {
	entries, err := os.ReadDir(s.cacheDir)
	if err != nil {
		s.log.Error("Failed to list cache dir for cleanup", "dir", s.cacheDir, "error", err)
		return
	}

	cutoff := time.Now().Add(-2 * 24 * time.Hour)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "kwh-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			full := filepath.Join(s.cacheDir, e.Name())
			if err := os.Remove(full); err == nil {
				s.log.Info("Automatically deleted old file", "file", e.Name())
			}
		}
	}
}
