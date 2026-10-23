package services

import (
	"net/http"
	"strconv"
	"time"

	"github.com/martinezsaweczko/claudeai-api/o11"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// NewExternalClient creates an HTTP client for external API calls with:
// - otelhttp transport (trace spans named "{method} {service}" + W3C propagation)
// - external_api_calls_total and external_api_duration_seconds metrics per service/target
func NewExternalClient(serviceName string, timeout time.Duration, m *o11.ServiceMetrics) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &instrumentedTransport{
			base: otelhttp.NewTransport(http.DefaultTransport,
				otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
					return r.Method + " " + serviceName
				}),
			),
			service: serviceName,
			metrics: m,
		},
	}
}

// instrumentedTransport records metrics for every outbound request
type instrumentedTransport struct {
	base    http.RoundTripper
	service string
	metrics *o11.ServiceMetrics
}

func (t *instrumentedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	duration := time.Since(start).Seconds()

	status := "error"
	if err == nil {
		status = strconv.Itoa(resp.StatusCode)
	}

	attrs := metric.WithAttributes(
		attribute.String("service", t.service),
		attribute.String("target", req.URL.Host),
		attribute.String("status", status),
	)
	t.metrics.ExternalCallsTotal.Add(req.Context(), 1, attrs)
	t.metrics.ExternalDuration.Record(req.Context(), duration, attrs)

	return resp, err
}
