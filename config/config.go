package config

import (
	"flag"
	"fmt"
	"strings"
)

type Config struct {
	HttpServer     ServerConfig // Public file server (JWT protected downloads)
	InternalServer ServerConfig // Internal API server (subscription trigger, metrics)
	Log            LogConfig
	O11            O11Config
	JWT            JWTConfig
	Bot            BotConfig
}

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

	// Define command-line flags for HTTP server configuration (public file server)
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

	// Define command-line flags for JWT configuration
	var jwtPublicKeyPath string
	var jwtPrivateKeyPath string

	// Define command-line flags for Bot configuration
	var botNumber, mentionedBotNumber, toNotification, trollNumbers string
	var nacionalFolder, internacionalFolder, magazineFolder string
	var retentionNacional, retentionInternacional, retentionMagazine int
	var urlServer, dbPath, dbDSN, dbDriver, sessionDBPath string
	var dbAutoCreate bool
	var openAIAPIKey, openAIModel string
	var weatherAPIKey, weatherAPIURL, weatherForecastAPIURL string
	var weatherNoteWords int
	var quoteAPIKey, quoteAPIURL string
	var stabilityAPIKey, esiosAPIKey string
	var commandTimeout int
	var pairPhone, electricityCacheDir string
	var pdfChannelJID, pdfCategory string
	var schedulerTimezone string
	var schedulerEnabled bool

	// Flags public file server
	flag.StringVar(&serverAddress, "http-address", "0.0.0.0", "Public file server address")
	flag.IntVar(&serverPort, "http-port", 46564, "Public file server port")
	flag.IntVar(&serverTimeout, "http-timeout", 30, "HTTP server timeout in seconds")
	flag.IntVar(&serverReadTimeout, "http-read-timeout", 10, "HTTP server read timeout in seconds")
	flag.IntVar(&serverWriteTimeout, "http-write-timeout", 30, "HTTP server write timeout in seconds")

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

	// JWT flags
	flag.StringVar(&jwtPublicKeyPath, "jwt-public-key-path", "keys/public.pem", "Path to JWT public key file")
	flag.StringVar(&jwtPrivateKeyPath, "jwt-private-key-path", "keys/private.pem", "Path to JWT private key file")

	// Bot flags
	flag.StringVar(&botNumber, "bot-number", "", "Bot phone number (e.g. 34936674253)")
	flag.StringVar(&mentionedBotNumber, "bot-mentioned-number", "", "Bot JID in @lid form for group mentions")
	flag.StringVar(&toNotification, "bot-notification-jid", "", "JID that receives restart notifications")
	flag.StringVar(&trollNumbers, "bot-troll-numbers", "", "Comma separated JIDs that get the troll AI personality")
	flag.StringVar(&nacionalFolder, "nacional-folder", "/tmp/periodico/nacional/", "National newspapers folder")
	flag.IntVar(&retentionNacional, "retention-nacional", 24, "National newspapers retention in hours")
	flag.StringVar(&internacionalFolder, "internacional-folder", "/tmp/periodico/internacional/", "International newspapers folder")
	flag.IntVar(&retentionInternacional, "retention-internacional", 169, "International newspapers retention in hours")
	flag.StringVar(&magazineFolder, "magazine-folder", "/tmp/periodico/magazine/", "Magazines folder")
	flag.IntVar(&retentionMagazine, "retention-magazine", 720, "Magazines retention in hours")
	flag.StringVar(&urlServer, "url-server", "localhost:46564", "Public base URL (host:port) for download links")
	flag.StringVar(&dbDriver, "db-driver", "sqlite", "Application database driver (sqlite or mysql)")
	flag.StringVar(&dbPath, "db-path", "/tmp/db.sqlite", "Application database path (sqlite only)")
	flag.StringVar(&dbDSN, "db-dsn", "", "Application database DSN (overrides db-path; required for mysql)")
	flag.BoolVar(&dbAutoCreate, "db-auto-create", true, "Automatically create the MySQL database if it does not exist")
	flag.StringVar(&sessionDBPath, "session-db-path", "/tmp/session.db", "WhatsApp session database path")
	flag.StringVar(&openAIAPIKey, "openai-api-key", "", "OpenAI API key")
	flag.StringVar(&openAIModel, "openai-model", "gpt-4o-mini", "OpenAI model")
	flag.StringVar(&weatherAPIKey, "weather-api-key", "", "OpenWeatherMap API key")
	flag.StringVar(&weatherAPIURL, "weather-api-url", "https://api.openweathermap.org/data/2.5/weather", "OpenWeatherMap API URL")
	flag.StringVar(&weatherForecastAPIURL, "weather-forecast-api-url", "https://api.openweathermap.org/data/2.5/forecast", "OpenWeatherMap 5 day / 3 hour forecast API URL")
	flag.IntVar(&weatherNoteWords, "weather-note-words", 200, "Max words in the weather note")
	flag.StringVar(&quoteAPIKey, "quote-api-key", "", "API Ninjas key for the quote command")
	flag.StringVar(&quoteAPIURL, "quote-api-url", "https://api.api-ninjas.com/v2/randomquotes", "API Ninjas random quotes API URL")
	flag.StringVar(&stabilityAPIKey, "stability-api-key", "", "Stability AI API key")
	flag.StringVar(&esiosAPIKey, "esios-api-key", "", "ESIOS (REE) API token")
	flag.IntVar(&commandTimeout, "command-timeout", 60, "Max execution time in seconds for a WhatsApp command")
	flag.StringVar(&pairPhone, "bot-pair-phone", "", "Phone number to pair with a code instead of QR (first login only)")
	flag.StringVar(&electricityCacheDir, "electricity-cache-dir", "/tmp", "Directory for electricity price cache files")
	flag.StringVar(&pdfChannelJID, "pdf-channel-jid", "", "WhatsApp group/channel JID that receives automatic PDF downloads")
	flag.StringVar(&pdfCategory, "pdf-category", "nacional", "Kiosk category where downloaded PDFs are saved")
	flag.StringVar(&schedulerTimezone, "scheduler-timezone", "Europe/Madrid", "Timezone used for scheduled command cron expressions")
	flag.BoolVar(&schedulerEnabled, "scheduler-enabled", true, "Enable the scheduled command runner")

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
		WriteTimeout: serverWriteTimeout,
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

	// Set JWT configuration
	config.JWT = JWTConfig{
		PublicKeyPath:  jwtPublicKeyPath,
		PrivateKeyPath: jwtPrivateKeyPath,
	}

	if err := config.JWT.LoadKeys(); err != nil {
		listErrors = append(listErrors, err)
	}

	// Set Bot configuration
	config.Bot = BotConfig{
		BotNumber:              botNumber,
		MentionedBotNumber:     mentionedBotNumber,
		ToNotification:         toNotification,
		TrollNumbers:           splitAndTrim(trollNumbers),
		NacionalFolder:         nacionalFolder,
		RetentionNacional:      retentionNacional,
		InternacionalFolder:    internacionalFolder,
		RetentionInternacional: retentionInternacional,
		MagazineFolder:         magazineFolder,
		RetentionMagazine:      retentionMagazine,
		URLServer:              urlServer,
		DBPath:                 dbPath,
		DBDriver:               dbDriver,
		DBDSN:                  dbDSN,
		DBAutoCreate:           dbAutoCreate,
		SessionDBPath:          sessionDBPath,
		OpenAIAPIKey:           openAIAPIKey,
		OpenAIModel:            openAIModel,
		WeatherAPIKey:          weatherAPIKey,
		WeatherAPIURL:          weatherAPIURL,
		WeatherForecastAPIURL:  weatherForecastAPIURL,
		WeatherNoteWords:       weatherNoteWords,
		QuoteAPIKey:            quoteAPIKey,
		QuoteAPIURL:            quoteAPIURL,
		StabilityAPIKey:        stabilityAPIKey,
		EsiosAPIKey:            esiosAPIKey,
		CommandTimeout:         commandTimeout,
		PairPhone:              pairPhone,
		ElectricityCacheDir:    electricityCacheDir,
		PDFChannelJID:          pdfChannelJID,
		PDFCategory:            pdfCategory,
		SchedulerTimezone:      schedulerTimezone,
		SchedulerEnabled:       schedulerEnabled,
	}

	if err := config.Bot.validate(); err != nil {
		listErrors = append(listErrors, err)
	}

	if len(listErrors) > 0 {
		return nil, &ConfigError{Message: "Invalid configuration", AppendedErrors: listErrors}
	}

	return config, nil
}

// splitAndTrim splits a comma separated list and trims whitespace, dropping empty entries
func splitAndTrim(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
