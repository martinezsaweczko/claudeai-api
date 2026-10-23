package o11

import (
	"go.opentelemetry.io/otel/metric"
)

// Meter names, one per application layer
const (
	MeterHTTP    = "whatsappbot/http"
	MeterCommand = "whatsappbot/command"
	MeterRepo    = "whatsappbot/repository"
	MeterService = "whatsappbot/service"
)

// HTTPMetrics holds the RED (rate, errors, duration) instruments for HTTP servers
type HTTPMetrics struct {
	RequestsTotal   metric.Int64Counter
	RequestDuration metric.Float64Histogram
	InFlight        metric.Int64UpDownCounter
}

func NewHTTPMetrics(mp metric.MeterProvider) (*HTTPMetrics, error) {
	m := mp.Meter(MeterHTTP)

	requestsTotal, err := m.Int64Counter("http_server_requests_total",
		metric.WithDescription("Total number of HTTP requests"))
	if err != nil {
		return nil, err
	}

	requestDuration, err := m.Float64Histogram("http_server_request_duration_seconds",
		metric.WithDescription("HTTP request duration in seconds"),
		metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}

	inFlight, err := m.Int64UpDownCounter("http_server_requests_in_flight",
		metric.WithDescription("Number of HTTP requests currently being served"))
	if err != nil {
		return nil, err
	}

	return &HTTPMetrics{
		RequestsTotal:   requestsTotal,
		RequestDuration: requestDuration,
		InFlight:        inFlight,
	}, nil
}

// CommandMetrics holds the instruments for WhatsApp command execution
type CommandMetrics struct {
	CommandsTotal   metric.Int64Counter
	CommandDuration metric.Float64Histogram
}

func NewCommandMetrics(mp metric.MeterProvider) (*CommandMetrics, error) {
	m := mp.Meter(MeterCommand)

	commandsTotal, err := m.Int64Counter("whatsapp_commands_total",
		metric.WithDescription("Total number of WhatsApp commands executed"))
	if err != nil {
		return nil, err
	}

	commandDuration, err := m.Float64Histogram("whatsapp_command_duration_seconds",
		metric.WithDescription("WhatsApp command execution duration in seconds"),
		metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}

	return &CommandMetrics{
		CommandsTotal:   commandsTotal,
		CommandDuration: commandDuration,
	}, nil
}

// RepoMetrics holds the instruments for repository (database) operations
type RepoMetrics struct {
	QueriesTotal  metric.Int64Counter
	QueryDuration metric.Float64Histogram
}

func NewRepoMetrics(mp metric.MeterProvider) (*RepoMetrics, error) {
	m := mp.Meter(MeterRepo)

	queriesTotal, err := m.Int64Counter("repo_queries_total",
		metric.WithDescription("Total number of repository operations"))
	if err != nil {
		return nil, err
	}

	queryDuration, err := m.Float64Histogram("repo_query_duration_seconds",
		metric.WithDescription("Repository operation duration in seconds"),
		metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}

	return &RepoMetrics{
		QueriesTotal:  queriesTotal,
		QueryDuration: queryDuration,
	}, nil
}

// ServiceMetrics holds the instruments for outbound external API calls
type ServiceMetrics struct {
	ExternalCallsTotal metric.Int64Counter
	ExternalDuration   metric.Float64Histogram
}

func NewServiceMetrics(mp metric.MeterProvider) (*ServiceMetrics, error) {
	m := mp.Meter(MeterService)

	externalCallsTotal, err := m.Int64Counter("external_api_calls_total",
		metric.WithDescription("Total number of calls to external APIs"))
	if err != nil {
		return nil, err
	}

	externalDuration, err := m.Float64Histogram("external_api_duration_seconds",
		metric.WithDescription("External API call duration in seconds"),
		metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}

	return &ServiceMetrics{
		ExternalCallsTotal: externalCallsTotal,
		ExternalDuration:   externalDuration,
	}, nil
}

// NewBusinessCounter creates a single counter in the service meter for a business event.
// Business counters are created where they are used (kiosk, subscriptions, electricity...)
func NewBusinessCounter(mp metric.MeterProvider, name, description string) (metric.Int64Counter, error) {
	return mp.Meter(MeterService).Int64Counter(name, metric.WithDescription(description))
}
