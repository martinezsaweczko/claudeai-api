# Grafana Dashboard - claudeai-api Operations

This folder contains the Grafana dashboard model for the `claudeai-api` service.

## Files

- `dashboard.json` – Grafana dashboard (schema v40). Import it via **Dashboards → Import** in Grafana.
- `alerts.example.yml` – example Prometheus alert rules (an example to adapt, not tuned values).
- `dashboard_test.go` – checks the dashboard structure and that every metric and label used by the dashboard and the alert rules exists in the app's real Prometheus exposition.

## Metrics used

The dashboard consumes Prometheus metrics exposed by the service on its single HTTP
port (default 46564, path `/metrics`, configurable with `-o11-prometheus-path`).

### Variables

| Variable | Description |
|----------|-------------|
| `datasource` | Prometheus data source. |
| `model` | Multi-value query variable (`label_values(claude_runs_total, model)`, includes All) that filters the Claude runs and tokens panels by the `model` label. Only series that already exist appear in the list. |
| `job` | Hidden regex for the scraped `job` label (defaults to `.+`). Change it via the dashboard settings if your Prometheus job name differs. It is applied to the Go runtime panels only, like the original dashboard; the application metrics are not filtered by job. |

### Dashboard rows

1. **Executive Summary** - active HTTP requests, Claude success ratio over the time range, Claude runs in flight and skills loaded.
2. **Claude Runs** - runs/s by outcome, success ratio (5m), errors by outcome, run duration p50/p95/p99, time to first token p50/p95, queue wait p95 with runs in flight.
3. **Tokens & Cost** - tokens/s by type, estimated cost (last 24h and per hour by model), cache hit ratio, average prompt tokens per run.
4. **Skills** - skills loaded, seconds since the last successful sync, consecutive sync failures, current commit/ref, sync outcomes and duration, skill invocations.
5. **HTTP Server** - request rate, status codes and p95 latency.
6. **External APIs** - outbound API call rate, status and latency per dependency.
7. **System Health** - goroutines, memory RSS and GC duration.

Cost panels show the Claude CLI's list-price estimate (`total_cost_usd`), not billing. Token panels sum every turn of a run, so a run that uses a skill counts more than its final prompt. The Skills row has no data when `-skills-repo` is not set.

## Importing

1. Open Grafana and go to **Dashboards → Import**.
2. Upload `dashboard.json` or paste its contents.
3. Select your Prometheus data source.
4. Save.

## Prometheus scrape requirements

Make sure Prometheus scrapes the service's HTTP port (the same one as the API), e.g.:

```yaml
scrape_configs:
  - job_name: 'claudeai-api'
    static_configs:
      - targets: ['localhost:46564']
    metrics_path: '/metrics'
    # Only when CLAUDEAI_API_KEYS is set: /metrics then requires a Bearer key.
    authorization:
      type: Bearer
      credentials: <one of the CLAUDEAI_API_KEYS>   # or credentials_file: /path/to/key
```

The port is configured with `-http-port` (default `46564`). Without
`CLAUDEAI_API_KEYS`, `/metrics` is open: keep the port on a private interface.

## Example alert rules

`alerts.example.yml` is a Prometheus rule file with the group `claudeai-api`: `ClaudeAuthErrors`, `ClaudeRateLimited`, `ClaudeErrorRatioHigh`, `ClaudeTimeToFirstTokenHigh`, `ClaudeBusyRejections`, `SkillsSyncFailing`, `SkillsStale` and `GatewayDown`. Thresholds and severities are starting points. `GatewayDown` uses the placeholder `job="claudeai-api"`; set it to your scrape job name. To load it, copy the file next to your Prometheus configuration and reference it:

```yaml
rule_files:
  - /etc/prometheus/rules/claudeai-api.alerts.yml
```

Validate it with `promtool check rules alerts.example.yml` and reload Prometheus.

## Keeping it in sync with the code

`go test ./grafana/` builds the real Claude and skills instruments, scrapes them through the Prometheus exporter and fails if a dashboard query, the `model` variable or an alert rule uses a metric or label that is not exposed. It only knows the `claude_*` and `skills_*` names; the template metrics (`http_server_*`, `external_api_*`, `go_*`, `process_*`) and `up` are on an explicit allowlist in the test. When you add or rename a metric, update the dashboard and keep that test green.

Rendering is not verified in CI: there is no Grafana in this repository's pipeline, so the layout and panel options were checked only structurally (valid JSON, unique ids, no overlapping grid positions). Import the dashboard into a Grafana 10/11 instance and eyeball it after changing panels.

## Notes

- The dashboard uses the metric names emitted by the OpenTelemetry Prometheus exporter, e.g. `http_server_requests_total`, `external_api_calls_total`, etc.
- All counter-based panels use `increase(...[$__range])` for the selected time range, while rate panels use a 5-minute window.
- Percentile panels use `$__rate_interval` so they adapt automatically to the dashboard time range.
