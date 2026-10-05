package main

import (
	"context"
	"errors"
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
	"seat-reservation/internal/reservation"
	"seat-reservation/internal/show"
	"seat-reservation/internal/user"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuration failed", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := database.Open(ctx, cfg.DatabaseURL, cfg.DatabaseMaxConn, logger)
	if err != nil {
		logger.Error("database startup failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := database.Migrate(ctx, pool, cfg.MigrationsDir); err != nil {
		logger.Error("database migration failed", "error", err)
		os.Exit(1)
	}

	tokenManager, err := auth.NewTokenManager(cfg.JWTSecret, "seat-reservation", cfg.TokenTTL)
	if err != nil {
		logger.Error("token manager startup failed", "error", err)
		os.Exit(1)
	}

	userService := user.NewService(pool, tokenManager)
	if err := userService.BootstrapAdmin(ctx, cfg.AdminEmail, cfg.AdminPassword); err != nil {
		logger.Error("admin bootstrap failed", "error", err)
		os.Exit(1)
	}
	showService := show.NewService(pool)
	reservationService := reservation.NewService(pool)

	server := &http.Server{
		Addr: cfg.Address,
		Handler: api.NewRouter(
			pool,
			logger,
			tokenManager,
			userService,
			showService,
			reservationService,
			cfg.AuthRate,
			cfg.AuthBurst,
		),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("http server starting", "address", cfg.Address)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown requested")
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			os.Exit(1)
		}
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownContext); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}

	logger.Info("shutdown complete")
}
