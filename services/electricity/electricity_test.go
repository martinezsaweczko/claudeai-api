package electricity

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func newTestService(t *testing.T, client *http.Client) *Service {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc, err := New(client, "test-api-key", t.TempDir(), log, tracenoop.NewTracerProvider(), noop.NewMeterProvider())
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}
	return svc
}

func TestSplitByDate(t *testing.T) {
	today := time.Now().UTC()
	tomorrow := today.Add(24 * time.Hour)

	points := []PricePoint{
		{Datetime: today, Value: 100},
		{Datetime: tomorrow, Value: 200},
		{Datetime: today.Add(48 * time.Hour), Value: 300}, // neither
	}

	todayStr := today.Format("2006-01-02")
	tomorrowStr := tomorrow.Format("2006-01-02")

	gotToday, gotTomorrow := splitByDate(points, todayStr, tomorrowStr)
	if len(gotToday) != 1 || gotToday[0].Value != 100 {
		t.Errorf("unexpected today points: %+v", gotToday)
	}
	if len(gotTomorrow) != 1 || gotTomorrow[0].Value != 200 {
		t.Errorf("unexpected tomorrow points: %+v", gotTomorrow)
	}
}

func TestIsNextDayFetchWindow(t *testing.T) {
	madrid := madridLocation()
	cases := []struct {
		hour int
		want bool
	}{
		{18, false}, {19, true}, {20, true}, {22, true}, {23, false}, {0, false},
	}
	for _, tc := range cases {
		now := time.Date(2024, 6, 1, tc.hour, 0, 0, 0, madrid)
		if got := isNextDayFetchWindow(now); got != tc.want {
			t.Errorf("hour %d: got %v, want %v", tc.hour, got, tc.want)
		}
	}
}

func TestTierForPrice(t *testing.T) {
	if tierForPrice(0.05) != colorCheap {
		t.Error("0.05 should be cheap")
	}
	if tierForPrice(0.12) != colorNormal {
		t.Error("0.12 should be normal")
	}
	if tierForPrice(0.20) != colorExpensive {
		t.Error("0.20 should be expensive")
	}
}

func TestBuildTierRunsConnected(t *testing.T) {
	// cheap, cheap, normal, expensive, expensive
	prices := []float64{0.05, 0.07, 0.12, 0.20, 0.25}
	runs := buildTierRuns(prices)

	if len(runs) != 3 {
		t.Fatalf("expected 3 runs, got %d", len(runs))
	}
	// Runs share boundary points to keep the line connected
	if runs[0].color != colorCheap || len(runs[0].xValues) != 3 { // 0.05, 0.07 + boundary 0.12
		t.Errorf("unexpected first run: %+v", runs[0])
	}
	if runs[1].color != colorNormal || len(runs[1].xValues) != 2 { // 0.12 + boundary 0.20
		t.Errorf("unexpected second run: %+v", runs[1])
	}
	if runs[2].color != colorExpensive || len(runs[2].xValues) != 2 {
		t.Errorf("unexpected third run: %+v", runs[2])
	}

	// Legend names: first run of each tier has a name
	names := 0
	for _, r := range runs {
		if r.name != "" {
			names++
		}
	}
	if names != 3 {
		t.Errorf("expected 3 legend names, got %d", names)
	}
}

func TestCacheRoundtrip(t *testing.T) {
	svc := newTestService(t, http.DefaultClient)
	now := time.Now().UTC()
	dateStr := now.Format("2006-01-02")

	points := []PricePoint{
		{Datetime: now, Value: 123.456},
		{Datetime: now.Add(time.Hour), Value: 78.9},
	}

	if svc.cacheExists(dateStr) {
		t.Fatal("cache should not exist yet")
	}

	if _, err := svc.saveCache(dateStr, points); err != nil {
		t.Fatalf("saveCache failed: %v", err)
	}
	if !svc.cacheExists(dateStr) {
		t.Fatal("cache should exist after save")
	}

	loaded, err := svc.loadCache(dateStr)
	if err != nil {
		t.Fatalf("loadCache failed: %v", err)
	}
	if len(loaded.Data) != 2 {
		t.Fatalf("expected 2 cached points, got %d", len(loaded.Data))
	}
	if loaded.Data[0].RawPrice != 123.456 {
		t.Errorf("unexpected raw price: %v", loaded.Data[0].RawPrice)
	}
	if loaded.tomorrowFetched() {
		t.Error("tomorrowFetched should be false for fresh cache")
	}

	// Metadata update
	svc.updateTomorrowMetadata(dateStr, true)
	loaded, _ = svc.loadCache(dateStr)
	if !loaded.tomorrowFetched() {
		t.Error("tomorrowFetched should be true after metadata update")
	}

	// toPoints roundtrip
	restored := loaded.toPoints()
	if len(restored) != 2 || restored[0].Value != 123.456 {
		t.Errorf("unexpected restored points: %+v", restored)
	}
}

// fakeESIOS serves a canned ESIOS response for the given points
func fakeESIOS(t *testing.T, points []PricePoint) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-api-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var resp esiosResponse
		for _, p := range points {
			resp.Indicator.Values = append(resp.Indicator.Values, struct {
				Value    float64   `json:"value"`
				Datetime time.Time `json:"datetime"`
			}{p.Value, p.Datetime})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestFetch(t *testing.T) {
	now := time.Now().UTC()
	server := fakeESIOS(t, []PricePoint{{Datetime: now, Value: 100}})
	defer server.Close()

	svc := newTestService(t, server.Client())
	svc.baseURL = server.URL + "/"

	points, err := svc.fetch(context.Background(), now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if len(points) != 1 || points[0].Value != 100 {
		t.Errorf("unexpected points: %+v", points)
	}
}

func TestBuildChartDataCacheMiss(t *testing.T) {
	now := time.Now().UTC()
	todayStr := now.Format("2006-01-02")

	var points []PricePoint
	for h := 0; h < 24; h++ {
		points = append(points, PricePoint{
			Datetime: time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, time.UTC),
			Value:    100 + float64(h),
		})
	}

	server := fakeESIOS(t, points)
	defer server.Close()

	svc := newTestService(t, server.Client())
	svc.baseURL = server.URL + "/"

	data, err := svc.buildChartData(context.Background(), todayStr, now.Add(24*time.Hour).Format("2006-01-02"))
	if err != nil {
		t.Fatalf("buildChartData failed: %v", err)
	}

	if data.IsDualDay {
		t.Error("should be single day")
	}
	if len(data.Points) != 24 {
		t.Errorf("expected 24 points, got %d", len(data.Points))
	}

	// Cache was written
	if !svc.cacheExists(todayStr) {
		t.Error("cache should exist after fetch")
	}

	// Second call: cache hit, no API needed
	server.Close()
	data, err = svc.buildChartData(context.Background(), todayStr, now.Add(24*time.Hour).Format("2006-01-02"))
	if err != nil {
		t.Fatalf("buildChartData from cache failed: %v", err)
	}
	if len(data.Points) != 24 {
		t.Errorf("expected 24 points from cache, got %d", len(data.Points))
	}
}

func TestRenderChart(t *testing.T) {
	svc := newTestService(t, http.DefaultClient)
	now := time.Now().UTC()

	var points []PricePoint
	for h := 0; h < 24; h++ {
		points = append(points, PricePoint{
			Datetime: time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, time.UTC),
			Value:    90 + float64(h%5)*30, // mix of tiers
		})
	}

	png, err := svc.renderChart(&chartData{TodayDate: now.Format("2006-01-02"), Points: points})
	if err != nil {
		t.Fatalf("renderChart failed: %v", err)
	}

	// PNG magic bytes
	if len(png) < 8 || png[0] != 0x89 || png[1] != 'P' || png[2] != 'N' || png[3] != 'G' {
		t.Fatal("output is not a PNG")
	}

	// Dual day
	var dual []PricePoint
	dual = append(dual, points...)
	for h := 0; h < 24; h++ {
		dual = append(dual, PricePoint{
			Datetime: time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, time.UTC).Add(24 * time.Hour),
			Value:    80 + float64(h),
		})
	}
	png, err = svc.renderChart(&chartData{
		TodayDate: now.Format("2006-01-02"), TomorrowDate: now.Add(24 * time.Hour).Format("2006-01-02"),
		Points: dual, IsDualDay: true,
	})
	if err != nil {
		t.Fatalf("renderChart dual failed: %v", err)
	}
	if len(png) < 8 || png[0] != 0x89 {
		t.Fatal("dual output is not a PNG")
	}
}

func TestRenderChartConcurrent(t *testing.T) {
	svc := newTestService(t, http.DefaultClient)

	// A fixed past day keeps the "current hour" marker out of every render,
	// so the serial and concurrent outputs are comparable byte for byte
	day := time.Date(2024, time.January, 15, 0, 0, 0, 0, time.UTC)
	var points []PricePoint
	for h := 0; h < 24; h++ {
		points = append(points, PricePoint{
			Datetime: day.Add(time.Duration(h) * time.Hour),
			Value:    90 + float64(h%5)*30, // mix of tiers
		})
	}
	data := &chartData{TodayDate: day.Format("2006-01-02"), Points: points}

	want, err := svc.renderChart(data)
	if err != nil {
		t.Fatalf("serial renderChart failed: %v", err)
	}

	const workers = 8
	results := make([][]byte, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.renderChart(data)
		}(i)
	}
	wg.Wait()

	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Errorf("render %d failed: %v", i, errs[i])
			continue
		}
		if !bytes.Equal(results[i], want) {
			t.Errorf("render %d differs from the serial render", i)
		}
	}
}
