package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/martinezsaweczko/whatsappBot-golang/http/handlers"
	"github.com/martinezsaweczko/whatsappBot-golang/http/middleware"
	"github.com/martinezsaweczko/whatsappBot-golang/o11"
	"github.com/martinezsaweczko/whatsappBot-golang/repository"
	"github.com/martinezsaweczko/whatsappBot-golang/services"
	"github.com/martinezsaweczko/whatsappBot-golang/services/ai"
	"github.com/martinezsaweczko/whatsappBot-golang/services/channelpdf"
	"github.com/martinezsaweczko/whatsappBot-golang/services/electricity"
	"github.com/martinezsaweczko/whatsappBot-golang/services/imagegen"
	"github.com/martinezsaweczko/whatsappBot-golang/services/kiosk"
	"github.com/martinezsaweczko/whatsappBot-golang/services/quote"
	"github.com/martinezsaweczko/whatsappBot-golang/services/scheduler"
	"github.com/martinezsaweczko/whatsappBot-golang/services/subscription"
	"github.com/martinezsaweczko/whatsappBot-golang/services/tts"
	"github.com/martinezsaweczko/whatsappBot-golang/services/weather"
	"github.com/martinezsaweczko/whatsappBot-golang/whatsapp"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.mau.fi/whatsmeow/types"
)

// initAndWire creates all services and handlers, registers HTTP routes and
// WhatsApp commands
func (app *App) initAndWire() error {
	cfg := app.config
	log := app.log
	tp := app.observability.Trace
	mp := app.observability.MeterProvider

	// ----- Metrics instruments -----
	httpMetrics, err := o11.NewHTTPMetrics(mp)
	if err != nil {
		return fmt.Errorf("failed to create HTTP metrics: %w", err)
	}
	repoMetrics, err := o11.NewRepoMetrics(mp)
	if err != nil {
		return fmt.Errorf("failed to create repo metrics: %w", err)
	}
	serviceMetrics, err := o11.NewServiceMetrics(mp)
	if err != nil {
		return fmt.Errorf("failed to create service metrics: %w", err)
	}
	jwtRejected, err := o11.NewBusinessCounter(mp, "jwt_tokens_rejected_total", "Rejected download tokens per reason")
	if err != nil {
		return fmt.Errorf("failed to create jwt rejected counter: %w", err)
	}

	// ----- Repository (instrumented) -----
	repo := repository.NewInstrumented(app.repo, repoMetrics, tp)

	// ----- JWT service -----
	jwtService := services.NewJWTService(
		cfg.JWT.PrivateKey,
		cfg.JWT.PublicKey,
		"whatsappbot-golang",
		24*time.Hour,
	)

	// ----- Kiosk categories -----
	hour := time.Hour
	categories := []kiosk.Category{
		{
			Name: "nacional", URLPrefix: "nacional_folder", Dir: cfg.Bot.NacionalFolder,
			Retention: time.Duration(cfg.Bot.RetentionNacional) * hour,
			Singular:  "periodico", Plural: "periodicos y revistas a nivel nacional",
			CommandWord: "periodico", RequestExample: "periodico:2",
		},
		{
			Name: "internacional", URLPrefix: "internacional_folder", Dir: cfg.Bot.InternacionalFolder,
			Retention: time.Duration(cfg.Bot.RetentionInternacional) * hour,
			Singular:  "periodico", Plural: "periodicos a nivel internacional",
			CommandWord: "newspaper", RequestExample: "newspaper:2",
		},
		{
			Name: "magazine", URLPrefix: "magazine_folder", Dir: cfg.Bot.MagazineFolder,
			Retention: time.Duration(cfg.Bot.RetentionMagazine) * hour,
			Singular:  "revista", Plural: "revistas a nivel internacional",
			CommandWord: "magazine", RequestExample: "magazine:2",
		},
	}

	fileServerCats := make(map[string]string, len(categories))
	urlPrefixes := make(map[string]string, len(categories))
	pdfFolders := make(map[string]string, len(categories))
	for _, c := range categories {
		fileServerCats[c.URLPrefix] = c.Dir
		urlPrefixes[c.Name] = c.URLPrefix
		pdfFolders[c.Name] = c.Dir
	}

	// ----- Services -----
	kioskSvc, err := kiosk.New(categories, cfg.Bot.URLServer, repo, jwtService, log, tp, mp)
	if err != nil {
		return fmt.Errorf("failed to create kiosk service: %w", err)
	}

	subsSvc, err := subscription.New(repo, jwtService, cfg.Bot.URLServer, urlPrefixes, log, tp, mp)
	if err != nil {
		return fmt.Errorf("failed to create subscription service: %w", err)
	}

	var channelpdfHandler whatsapp.ChannelMediaHandler
	if cfg.Bot.PDFChannelJID != "" {
		channelpdfSvc, err := channelpdf.New(cfg.Bot.PDFChannelJID, cfg.Bot.PDFCategory, pdfFolders, app.waClient, subsSvc, log, tp, mp)
		if err != nil {
			return fmt.Errorf("failed to create channel pdf service: %w", err)
		}
		channelpdfHandler = channelpdfSvc.Handler()
	}

	ttsSvc, err := tts.New(services.NewExternalClient("gtts", 30*time.Second, serviceMetrics), log, tp, mp)
	if err != nil {
		return fmt.Errorf("failed to create tts service: %w", err)
	}

	aiSvc, err := ai.New(
		cfg.Bot.OpenAIAPIKey, cfg.Bot.OpenAIModel,
		services.NewExternalClient("openai", 60*time.Second, serviceMetrics),
		cfg.Bot.TrollNumbers, ttsSvc, log, tp, mp,
	)
	if err != nil {
		return fmt.Errorf("failed to create ai service: %w", err)
	}

	weatherSvc := weather.New(
		services.NewExternalClient("openweather", 15*time.Second, serviceMetrics),
		cfg.Bot.WeatherAPIURL, cfg.Bot.WeatherForecastAPIURL, cfg.Bot.WeatherAPIKey, cfg.Bot.WeatherNoteWords,
		aiSvc, log, tp,
	)

	quoteSvc := quote.New(
		services.NewExternalClient("apininjas", 15*time.Second, serviceMetrics),
		cfg.Bot.QuoteAPIURL, cfg.Bot.QuoteAPIKey,
		aiSvc, log, tp,
	)

	imagegenSvc, err := imagegen.New(
		services.NewExternalClient("stability", 120*time.Second, serviceMetrics),
		cfg.Bot.StabilityAPIKey, log, tp, mp,
	)
	if err != nil {
		return fmt.Errorf("failed to create imagegen service: %w", err)
	}

	electricitySvc, err := electricity.New(
		services.NewExternalClient("esios", 30*time.Second, serviceMetrics),
		cfg.Bot.EsiosAPIKey, cfg.Bot.ElectricityCacheDir, log, tp, mp,
	)
	if err != nil {
		return fmt.Errorf("failed to create electricity service: %w", err)
	}

	var schedulerSvc *scheduler.Service
	if cfg.Bot.SchedulerEnabled {
		schedulerSvc, err = scheduler.New(
			repo,
			app.router,
			app.waClient,
			log,
			tp,
			scheduler.Config{
				Timezone: cfg.Bot.SchedulerTimezone,
				Enabled:  cfg.Bot.SchedulerEnabled,
			},
		)
		if err != nil {
			return fmt.Errorf("failed to create scheduler service: %w", err)
		}
	}

	// ----- HTTP handlers -----
	healthHandlerConf := handlers.HealthHandlerConfig{
		Log:           log,
		Observability: app.observability,
		Cfg:           cfg,
		BasePath:      app.basePath,
	}
	healthHandler, err := healthHandlerConf.NewHealthHandler()
	if err != nil {
		return fmt.Errorf("failed to create health handler: %w", err)
	}

	versionHandlerConf := handlers.VersionHandlerConfig{
		Log:           log,
		Observability: app.observability,
		Cfg:           cfg,
		AppBuildInfo:  app.buildInfo,
		BasePath:      app.basePath,
	}
	versionHandler, err := versionHandlerConf.NewVersionHandler()
	if err != nil {
		return fmt.Errorf("failed to create version handler: %w", err)
	}

	metricsHandlerConf := handlers.MetricsHandlerConfig{
		Log:           log,
		Observability: app.observability,
		Cfg:           cfg,
	}
	metricsHandler := metricsHandlerConf.NewMetricsHandler()

	fileServerConf := handlers.FileServerHandlerConfig{
		Log:        log,
		TP:         tp,
		MP:         mp,
		Usage:      repo,
		Categories: fileServerCats,
	}
	fileServerHandler, err := fileServerConf.NewFileServerHandler()
	if err != nil {
		return fmt.Errorf("failed to create file server handler: %w", err)
	}

	triggerConf := handlers.SubscriptionTriggerHandlerConfig{
		Log:      log,
		TP:       tp,
		Notifier: subsSvc,
		Sender:   app.waClient,
		BasePath: app.basePath,
	}
	triggerHandler := triggerConf.NewSubscriptionTriggerHandler()

	swaggerHandler := handlers.NewSwaggerHandler(log, app.basePath)

	notificationConf := handlers.NotificationHandlerConfig{
		Log:      log,
		Sender:   app.waClient,
		BasePath: app.basePath,
	}
	notificationHandler, err := notificationConf.NewNotificationHandler()
	if err != nil {
		return fmt.Errorf("failed to create notification handler: %w", err)
	}

	whatsappConf := handlers.WhatsAppHandlerConfig{
		Log:         log,
		GroupClient: app.waClient,
		BasePath:    app.basePath,
	}
	whatsappHandler, err := whatsappConf.NewWhatsAppHandler()
	if err != nil {
		return fmt.Errorf("failed to create whatsapp handler: %w", err)
	}

	var schedulerHandler *handlers.SchedulerHandler
	if schedulerSvc != nil {
		schedulerConf := handlers.SchedulerHandlerConfig{
			Log:      log,
			Service:  schedulerSvc,
			BasePath: app.basePath,
		}
		schedulerHandler, err = schedulerConf.NewSchedulerHandler()
		if err != nil {
			return fmt.Errorf("failed to create scheduler handler: %w", err)
		}
	}

	// ----- Routes -----
	tracingMiddleware := middleware.TracingMiddleware()
	metricsMiddleware := middleware.MetricsMiddleware(httpMetrics)
	loggingMiddleware := middleware.LoggingMiddleware(log)
	jwtQueryMiddleware := middleware.JWTQueryMiddleware(log,
		func(token string) error {
			_, err := jwtService.VerifyToken(token)
			return err
		},
		repo, repo, jwtRejected)

	// Public file server: tracing -> metrics -> logging -> jwt auth -> handler
	fileServerHandler.RegisterRoutes(app.publicServer,
		tracingMiddleware, metricsMiddleware, loggingMiddleware, jwtQueryMiddleware)

	// Internal server: health, version, metrics, subscription trigger
	healthHandler.RegisterRoutes(app.internalServer)
	versionHandler.RegisterRoutes(app.internalServer, loggingMiddleware)
	if cfg.O11.PrometheusPath != "" {
		metricsHandler.RegisterRoutes(app.internalServer, promhttp.Handler())
	}
	triggerHandler.RegisterRoutes(app.internalServer,
		tracingMiddleware, metricsMiddleware, loggingMiddleware)
	swaggerHandler.RegisterRoutes(app.internalServer, loggingMiddleware)
	notificationHandler.RegisterRoutes(app.internalServer,
		tracingMiddleware, metricsMiddleware, loggingMiddleware)
	whatsappHandler.RegisterRoutes(app.internalServer,
		tracingMiddleware, metricsMiddleware, loggingMiddleware)
	if schedulerHandler != nil {
		schedulerHandler.RegisterRoutes(app.internalServer,
			tracingMiddleware, metricsMiddleware, loggingMiddleware)
	}

	// ----- WhatsApp commands -----
	for _, cmd := range kioskSvc.Commands(cfg.Bot.BotNumber, app.buildInfo.Version) {
		app.router.Register(cmd.Name, cmd.Pattern, cmd.Handler)
	}
	for _, cmd := range subsSvc.Commands() {
		app.router.Register(cmd.Name, cmd.Pattern, cmd.Handler)
	}
	for _, cmd := range ttsSvc.Commands() {
		app.router.Register(cmd.Name, cmd.Pattern, cmd.Handler)
	}
	for _, cmd := range weatherSvc.Commands() {
		app.router.Register(cmd.Name, cmd.Pattern, cmd.Handler)
	}
	for _, cmd := range imagegenSvc.Commands() {
		app.router.Register(cmd.Name, cmd.Pattern, cmd.Handler)
	}
	for _, cmd := range electricitySvc.Commands() {
		app.router.Register(cmd.Name, cmd.Pattern, cmd.Handler)
	}
	for _, cmd := range quoteSvc.Commands() {
		app.router.Register(cmd.Name, cmd.Pattern, cmd.Handler)
	}
	if cfg.Bot.PDFChannelJID != "" {
		if err := app.router.RegisterChannelMediaHandler(cfg.Bot.PDFChannelJID, channelpdfHandler); err != nil {
			return fmt.Errorf("failed to register channel media handler: %w", err)
		}
	}
	app.router.SetFallback(aiSvc.Fallback())
	app.router.SetSender(app.waClient)

	// ----- WhatsApp hooks -----
	app.router.SetConnectedHook(func(ctx context.Context) {
		jid, err := types.ParseJID(cfg.Bot.ToNotification)
		if err != nil {
			log.Error("Invalid notification JID", "jid", cfg.Bot.ToNotification, "error", err)
			return
		}
		message := "Application restarted at: " + time.Now().Format(time.RFC3339) +
			" with version number: " + app.buildInfo.Version
		if err := app.waClient.SendText(ctx, jid, message); err != nil {
			log.Error("Failed to send restart notification", "error", err)
		}
	})

	app.router.SetLoggedOutHook(func() {
		log.Error("Logged out from WhatsApp, exiting to trigger a fresh login")
		os.Exit(1)
	})

	if schedulerSvc != nil {
		app.WithScheduler(schedulerSvc)
	}

	log.Info("Application wired successfully",
		"commands", len(kioskSvc.Commands(cfg.Bot.BotNumber, app.buildInfo.Version))+
			len(subsSvc.Commands())+len(ttsSvc.Commands())+len(weatherSvc.Commands())+
			len(imagegenSvc.Commands())+len(electricitySvc.Commands())+len(quoteSvc.Commands()))

	return nil
}
