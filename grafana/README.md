# Grafana Dashboard – WhatsApp Bot Operations

This folder contains the Grafana dashboard model for the `whatsappBot-golang` service.

## Files

- `dashboard.json` – Grafana dashboard (schema v40). Import it via **Dashboards → Import** in Grafana.

## Metrics used

The dashboard consumes Prometheus metrics exposed by the bot on its internal
server (default path `/metrics`, configurable with `-o11-prometheus-path`).

### Variables

| Variable | Description |
|----------|-------------|
| `datasource` | Prometheus data source. |
| `command` | Multi-select filter for WhatsApp commands. Populated from `whatsapp_commands_total`. |
| `job` | Hidden regex for the scraped `job` label (defaults to `.+`). Change it via the dashboard settings if your Prometheus job name differs. |

### Dashboard rows

1. **Executive Summary** – total commands, throughput, errors, average duration, active HTTP requests, files served.
2. **WhatsApp Commands** – command usage over time, totals, errors and result distribution.
3. **Performance Percentiles** – p50/p90/p95/p99 durations, heatmap and per-command p95.
4. **Newspaper & Kiosk** – links generated, lists served, file downloads, channel PDF monitoring.
5. **Subscriptions** – subscription events and notifications sent.
6. **AI, Image, TTS & Electricity** – feature usage and electricity cache hit/miss.
7. **HTTP Server & JWT** – request rate, status codes, p95 latency and JWT rejection reasons.
8. **External APIs** – outbound API call rate, status and latency per dependency.
9. **Database / Repository** – DB operation rate, errors and latency.
10. **System Health** – goroutines, memory RSS and GC duration.

## Importing

1. Open Grafana and go to **Dashboards → Import**.
2. Upload `dashboard.json` or paste its contents.
3. Select your Prometheus data source.
4. Save.

## Prometheus scrape requirements

Make sure Prometheus scrapes the bot's internal HTTP endpoint, e.g.:

```yaml
scrape_configs:
  - job_name: 'whatsappbot'
    static_configs:
      - targets: ['localhost:9000']
    metrics_path: '/metrics'
```

The internal port is configured with `-internal-port` (default `9000`).

## Notes

- The dashboard uses the metric names emitted by the OpenTelemetry Prometheus exporter, e.g. `whatsapp_commands_total`, `http_server_requests_total`, etc.
- All counter-based panels use `increase(...[$__range])` for the selected time range, while rate panels use a 5-minute window.
- Percentile panels use `$__rate_interval` so they adapt automatically to the dashboard time range.
