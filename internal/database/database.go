package database

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"seat-reservation/internal/telemetry"

	"github.com/jackc/pgx/v5/pgxpool"
)

func Open(ctx context.Context, databaseURL string, maxConnections int32, logger *slog.Logger) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database configuration: %w", err)
	}

	poolConfig.ConnConfig.Tracer = telemetry.NewPGXTracer()
	poolConfig.MaxConns = maxConnections
	poolConfig.MinConns = min(maxConnections, 4)
	poolConfig.MaxConnLifetime = 30 * time.Minute
	poolConfig.MaxConnIdleTime = 5 * time.Minute
	poolConfig.HealthCheckPeriod = 30 * time.Second

	var pool *pgxpool.Pool
	for attempt := 1; attempt <= 20; attempt++ {
		pool, err = pgxpool.NewWithConfig(ctx, poolConfig)
		if err == nil {
			pingContext, cancel := context.WithTimeout(ctx, 2*time.Second)
			err = pool.Ping(pingContext)
			cancel()
		}
		if err == nil {
			return pool, nil
		}
		if pool != nil {
			pool.Close()
		}

		logger.Warn(
			"database connection attempt failed",
			"event", "database_connection_attempt_failed",
			"attempt", attempt,
			"error", err,
		)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}

	return nil, fmt.Errorf("connect to database: %w", err)
}
