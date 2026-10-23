package config

import (
	"flag"
	"fmt"
	"strings"
)

type Config struct {
	HttpServer     ServerConfig // Public API server
	InternalServer ServerConfig // Internal server (health, version, metrics, swagger)
	Log            LogConfig
	O11            O11Config
}

// internalWriteTimeoutSeconds is the fixed write timeout of the internal server,
// which only serves short responses. Only the public server may stream.
const internalWriteTimeoutSeconds = 30

type ConfigError struct {
	Message        string
	AppendedErrors []error
}

// Error implements the error interface for ConfigError
// It formats all validation errors into a single error message
func (ce *ConfigError) Error() string {
	if len(ce.AppendedErrors) == 0 {
		return ce.Message
	}

	var errMessages []string
	for _, err := range ce.AppendedErrors {
		errMessages = append(errMessages, err.Error())
	}

	return fmt.Sprintf("%s:\n  - %s", ce.Message, strings.Join(errMessages, "\n  - "))
}

// Load configuration from command-line flags
// Return the configuration struct
func LoadConfig() (*Config, *ConfigError) {
	config := &Config{}
	listErrors := []error{}

	// Define command-line flags for HTTP server configuration (public API server)
	var serverAddress string
	var serverPort int
	var serverTimeout int
	var serverReadTimeout int
	var serverWriteTimeout int

	// Define command-line flags for internal HTTP server configuration
	var internalAddress string
	var internalPort int

	// Define command-line flags for LOG configuration
	var logLevel string
	var logFilePath string

	// Define command-line flags for O11 configuration
	var o11TracerEndpoint string
	var o11PrometheusPath string
	var o11Environment string

	// Flags public API server
	flag.StringVar(&serverAddress, "http-address", "0.0.0.0", "Public API server address")
	flag.IntVar(&serverPort, "http-port", 46564, "Public API server port")
	flag.IntVar(&serverTimeout, "http-timeout", 30, "HTTP server timeout in seconds")
	flag.IntVar(&serverReadTimeout, "http-read-timeout", 10, "HTTP server read timeout in seconds")
	flag.IntVar(&serverWriteTimeout, "http-write-timeout", 0, "Public server write timeout in seconds (0 disables it, required for long-lived SSE streaming responses)")

	// Flags internal server
	flag.StringVar(&internalAddress, "internal-address", "0.0.0.0", "Internal API server address")
	flag.IntVar(&internalPort, "internal-port", 9000, "Internal API server port")

	// Flags log
	flag.StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	flag.StringVar(&logFilePath, "log-file-path", "stdout", "Log file path (default is stdout)")

	// O11 flags
	flag.StringVar(&o11TracerEndpoint, "o11-tracer-endpoint", "", "OpenTelemetry tracer endpoint (gRPC, without http:// or https://)")
	flag.StringVar(&o11PrometheusPath, "o11-prometheus-path", "/metrics", "OpenTelemetry Prometheus metrics path")
	flag.StringVar(&o11Environment, "o11-environment", "dev", "Deployment environment reported in telemetry (dev, prod)")

	flag.Parse()

	config.HttpServer.Address = serverAddress
	config.HttpServer.Port = serverPort
	config.HttpServer.Timeout = serverTimeout
	config.HttpServer.ReadTimeout = serverReadTimeout
	config.HttpServer.WriteTimeout = serverWriteTimeout

	if err := config.HttpServer.validate(); err != nil {
		listErrors = append(listErrors, err)
	}

	config.InternalServer = ServerConfig{
		Address:      internalAddress,
		Port:         internalPort,
		Timeout:      serverTimeout,
		ReadTimeout:  serverReadTimeout,
		WriteTimeout: internalWriteTimeoutSeconds,
	}

	if err := config.InternalServer.validate(); err != nil {
		listErrors = append(listErrors, err)
	}

	config.Log.Level = logLevel
	config.Log.FilePath = logFilePath

	if err := config.Log.validate(); err != nil {
		listErrors = append(listErrors, err)
	}

	// Set O11 configuration
	o11Conf := &O11Config{
		TracerEndpoint: o11TracerEndpoint,
		PrometheusPath: o11PrometheusPath,
		Environment:    o11Environment,
	}

	config.O11 = *o11Conf

	if err := o11Conf.Validate(); err != nil {
		listErrors = append(listErrors, err)
	}

	if len(listErrors) > 0 {
		return nil, &ConfigError{Message: "Invalid configuration", AppendedErrors: listErrors}
	}

	return config, nil
}
