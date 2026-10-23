package o11

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// newManualMeterProvider returns an SDK meter provider whose measurements can
// be collected on demand, without any exporter.
func newManualMeterProvider(t *testing.T) (*sdkmetric.MeterProvider, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	return mp, reader
}

// collectedMetrics collects once and indexes the metrics by scope (meter) name
// and then by metric name.
func collectedMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect failed: %v", err)
	}
	out := map[string]map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		if out[sm.Scope.Name] == nil {
			out[sm.Scope.Name] = map[string]metricdata.Metrics{}
		}
		for _, m := range sm.Metrics {
			out[sm.Scope.Name][m.Name] = m
		}
	}
	return out
}

// sumValue returns the total of an int64 sum metric across its data points.
func sumValue(t *testing.T, m metricdata.Metrics) int64 {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q is %T, want an int64 sum", m.Name, m.Data)
	}
	var total int64
	for _, dp := range sum.DataPoints {
		total += dp.Value
	}
	return total
}

// histogramCount returns the number of observations of a float64 histogram.
func histogramCount(t *testing.T, m metricdata.Metrics) uint64 {
	t.Helper()
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("metric %q is %T, want a float64 histogram", m.Name, m.Data)
	}
	var count uint64
	for _, dp := range hist.DataPoints {
		count += dp.Count
	}
	return count
}

// The metric names and meter names below are the contract with the Prometheus
// queries and dashboards built on top of them.
func TestMetricFactoriesExposeExpectedInstruments(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name       string
		meter      string
		record     func(t *testing.T, mp metric.MeterProvider)
		counters   map[string]int64
		histograms map[string]string // name -> unit
	}{
		{
			name:  "http",
			meter: MeterHTTP,
			record: func(t *testing.T, mp metric.MeterProvider) {
				m, err := NewHTTPMetrics(mp)
				if err != nil {
					t.Fatalf("NewHTTPMetrics failed: %v", err)
				}
				m.RequestsTotal.Add(ctx, 2)
				m.RequestDuration.Record(ctx, 0.25)
				m.InFlight.Add(ctx, 3)
				m.InFlight.Add(ctx, -1)
			},
			counters:   map[string]int64{"http_server_requests_total": 2, "http_server_requests_in_flight": 2},
			histograms: map[string]string{"http_server_request_duration_seconds": "s"},
		},
		{
			name:  "repository",
			meter: MeterRepo,
			record: func(t *testing.T, mp metric.MeterProvider) {
				m, err := NewRepoMetrics(mp)
				if err != nil {
					t.Fatalf("NewRepoMetrics failed: %v", err)
				}
				m.QueriesTotal.Add(ctx, 4)
				m.QueryDuration.Record(ctx, 0.01)
			},
			counters:   map[string]int64{"repo_queries_total": 4},
			histograms: map[string]string{"repo_query_duration_seconds": "s"},
		},
		{
			name:  "service",
			meter: MeterService,
			record: func(t *testing.T, mp metric.MeterProvider) {
				m, err := NewServiceMetrics(mp)
				if err != nil {
					t.Fatalf("NewServiceMetrics failed: %v", err)
				}
				m.ExternalCallsTotal.Add(ctx, 5)
				m.ExternalDuration.Record(ctx, 0.7)
			},
			counters:   map[string]int64{"external_api_calls_total": 5},
			histograms: map[string]string{"external_api_duration_seconds": "s"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mp, reader := newManualMeterProvider(t)
			tt.record(t, mp)

			scopes := collectedMetrics(t, reader)
			metrics, ok := scopes[tt.meter]
			if !ok {
				t.Fatalf("no metrics under meter %q, got scopes %v", tt.meter, scopes)
			}
			if want := len(tt.counters) + len(tt.histograms); len(metrics) != want {
				t.Errorf("meter %q has %d instruments, want %d", tt.meter, len(metrics), want)
			}

			for name, want := range tt.counters {
				m, ok := metrics[name]
				if !ok {
					t.Errorf("counter %q not exported", name)
					continue
				}
				if m.Description == "" {
					t.Errorf("counter %q has no description", name)
				}
				if got := sumValue(t, m); got != want {
					t.Errorf("counter %q = %d, want %d", name, got, want)
				}
			}
			for name, unit := range tt.histograms {
				m, ok := metrics[name]
				if !ok {
					t.Errorf("histogram %q not exported", name)
					continue
				}
				if m.Unit != unit {
					t.Errorf("histogram %q unit = %q, want %q", name, m.Unit, unit)
				}
				if got := histogramCount(t, m); got != 1 {
					t.Errorf("histogram %q has %d observations, want 1", name, got)
				}
			}
		})
	}
}

func TestNewBusinessCounter(t *testing.T) {
	ctx := context.Background()
	mp, reader := newManualMeterProvider(t)

	counter, err := NewBusinessCounter(mp, "requests_served_total", "Requests served")
	if err != nil {
		t.Fatalf("NewBusinessCounter failed: %v", err)
	}
	counter.Add(ctx, 1, metric.WithAttributes(attribute.String("category", "nacional")))
	counter.Add(ctx, 2, metric.WithAttributes(attribute.String("category", "magazine")))

	// Business counters share the service meter with the external API metrics.
	m, ok := collectedMetrics(t, reader)[MeterService]["requests_served_total"]
	if !ok {
		t.Fatalf("business counter not exported under meter %q", MeterService)
	}
	if m.Description != "Requests served" {
		t.Errorf("description = %q", m.Description)
	}
	sum := m.Data.(metricdata.Sum[int64])
	if !sum.IsMonotonic {
		t.Error("a business counter must be monotonic")
	}
	if len(sum.DataPoints) != 2 {
		t.Errorf("got %d data points, want one per category", len(sum.DataPoints))
	}
	if got := sumValue(t, m); got != 3 {
		t.Errorf("total = %d, want 3", got)
	}
}

func TestNewBusinessCounterRejectsInvalidName(t *testing.T) {
	mp, _ := newManualMeterProvider(t)

	// Instrument names must start with a letter.
	if _, err := NewBusinessCounter(mp, "1_invalid_name", "bad"); err == nil {
		t.Fatal("expected an error for an invalid instrument name")
	}
}
