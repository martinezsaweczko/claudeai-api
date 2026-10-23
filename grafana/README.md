# Grafana Dashboard - claudeai-api Operations

This folder contains the Grafana dashboard model for the `claudeai-api` service.

## Files

- `dashboard.json` – Grafana dashboard (schema v40). Import it via **Dashboards → Import** in Grafana.

## Metrics used

The dashboard consumes Prometheus metrics exposed by the service on its internal
server (default path `/metrics`, configurable with `-o11-prometheus-path`).

### Variables

| Variable | Description |
|----------|-------------|
| `datasource` | Prometheus data source. |
| `job` | Hidden regex for the scraped `job` label (defaults to `.+`). Change it via the dashboard settings if your Prometheus job name differs. |

### Dashboard rows

1. **Executive Summary** - active HTTP requests.
2. **HTTP Server** - request rate, status codes and p95 latency.
3. **External APIs** - outbound API call rate, status and latency per dependency.
4. **System Health** - goroutines, memory RSS and GC duration.

## Importing

1. Open Grafana and go to **Dashboards → Import**.
2. Upload `dashboard.json` or paste its contents.
3. Select your Prometheus data source.
4. Save.

## Prometheus scrape requirements

Make sure Prometheus scrapes the service's internal HTTP endpoint, e.g.:

```yaml
scrape_configs:
  - job_name: 'claudeai-api'
    static_configs:
      - targets: ['localhost:9000']
    metrics_path: '/metrics'
```

The internal port is configured with `-internal-port` (default `9000`).

## Notes

- The dashboard uses the metric names emitted by the OpenTelemetry Prometheus exporter, e.g. `http_server_requests_total`, `external_api_calls_total`, etc.
- All counter-based panels use `increase(...[$__range])` for the selected time range, while rate panels use a 5-minute window.
- Percentile panels use `$__rate_interval` so they adapt automatically to the dashboard time range.
