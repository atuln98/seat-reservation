package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"seat-reservation/internal/api"
	"seat-reservation/internal/auth"
	"seat-reservation/internal/config"
	"seat-reservation/internal/database"
	"seat-reservation/internal/logbroadcast"
	"seat-reservation/internal/metrics"
	"seat-reservation/internal/reservation"
	"seat-reservation/internal/show"
	"seat-reservation/internal/telemetry"
	"seat-reservation/internal/user"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

func main() {
	startupLogger := newLogger(os.Stdout)
	cfg, err := config.Load()
	if err != nil {
		startupLogger.Error("configuration failed", "event", "configuration_failed", "error", err)
		os.Exit(1)
	}

	broadcaster, err := logbroadcast.New(logbroadcast.Config{
		URL:            cfg.LokiPushURL,
		Username:       cfg.LokiUsername,
		Password:       cfg.LokiPassword,
		Environment:    cfg.LokiEnvironment,
		QueueBytes:     cfg.LokiQueueBytes,
		BatchBytes:     cfg.LokiBatchBytes,
		FlushInterval:  cfg.LokiFlushInterval,
		RequestTimeout: cfg.LokiRequestTimeout,
	})
	if err != nil {
		startupLogger.Error("log broadcaster startup failed", "event", "log_broadcaster_startup_failed", "error", err)
		os.Exit(1)
	}
	logOutput := io.Writer(os.Stdout)
	if broadcaster.Enabled() {
		logOutput = io.MultiWriter(broadcaster, os.Stdout)
	}
	logger := newLogger(logOutput)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	traceProvider, traceStats, err := telemetry.New(ctx, telemetry.Config{
		Endpoint:       cfg.OTLPEndpoint,
		Username:       cfg.OTLPUsername,
		Password:       cfg.OTLPPassword,
		Environment:    cfg.LokiEnvironment,
		RequestTimeout: cfg.OTLPRequestTimeout,
		SampleRatio:    cfg.OTLPSampleRatio,
		ExportWorkers:  cfg.OTLPExportWorkers,
		QueueSpans:     cfg.OTLPQueueSpans,
	})
	if err != nil {
		fatal(logger, broadcaster, "telemetry startup failed", "telemetry_startup_failed", err)
	}
	otel.SetTracerProvider(traceProvider)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	pool, err := database.Open(ctx, cfg.DatabaseURL, cfg.DatabaseMaxConn, logger)
	if err != nil {
		fatal(logger, broadcaster, "database startup failed", "database_startup_failed", err)
	}
	defer pool.Close()

	if err := database.Migrate(ctx, pool, cfg.MigrationsDir); err != nil {
		fatal(logger, broadcaster, "database migration failed", "database_migration_failed", err)
	}

	tokenManager, err := auth.NewTokenManager(cfg.JWTSecret, "seat-reservation", cfg.TokenTTL)
	if err != nil {
		fatal(logger, broadcaster, "token manager startup failed", "token_manager_startup_failed", err)
	}

	userService := user.NewService(pool, tokenManager)
	if err := userService.BootstrapAdmin(ctx, cfg.AdminEmail, cfg.AdminPassword); err != nil {
		fatal(logger, broadcaster, "admin bootstrap failed", "admin_bootstrap_failed", err)
	}
	showService := show.NewService(pool)
	reservationService := reservation.NewService(pool)
	applicationMetrics := metrics.New(pool)
	applicationMetrics.RegisterLogBroadcast(broadcaster)
	applicationMetrics.RegisterTraceExport(traceStats)

	server := &http.Server{
		Addr: cfg.Address,
		Handler: api.NewRouter(
			pool,
			logger,
			tokenManager,
			userService,
			showService,
			reservationService,
			applicationMetrics,
			cfg.AuthRate,
			cfg.AuthBurst,
			cfg.MetricsBearerToken,
			cfg.RequestTimeout,
		),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("http server starting", "event", "http_server_starting", "address", cfg.Address)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown requested", "event", "shutdown_requested")
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			fatal(logger, broadcaster, "http server failed", "http_server_failed", err)
		}
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownContext); err != nil {
		fatal(logger, broadcaster, "graceful shutdown failed", "graceful_shutdown_failed", err)
	}
	if err := traceProvider.Shutdown(shutdownContext); err != nil {
		logger.Error("telemetry shutdown failed", "event", "telemetry_shutdown_failed", "error", err)
	}

	logger.Info("shutdown complete", "event", "shutdown_complete")
	broadcastContext, broadcastCancel := context.WithTimeout(context.Background(), cfg.LokiRequestTimeout+time.Second)
	defer broadcastCancel()
	if err := broadcaster.Close(broadcastContext); err != nil {
		startupLogger.Error("log broadcaster shutdown failed", "event", "log_broadcaster_shutdown_failed", "error", err)
	}
}

func newLogger(output io.Writer) *slog.Logger {
	handler := slog.NewJSONHandler(output, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, attribute slog.Attr) slog.Attr {
			switch attribute.Key {
			case slog.TimeKey:
				attribute.Key = "timestamp"
			case slog.MessageKey:
				attribute.Key = "message"
			}
			return attribute
		},
	})
	return slog.New(handler).With("service", "seat-reservation")
}

func fatal(logger *slog.Logger, broadcaster *logbroadcast.Broadcaster, message string, event string, err error) {
	logger.Error(message, "event", event, "error", err)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	_ = broadcaster.Close(ctx)
	os.Exit(1)
}
