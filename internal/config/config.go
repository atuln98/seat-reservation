package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Address         string
	DatabaseURL     string
	JWTSecret       string
	AdminEmail      string
	AdminPassword   string
	MigrationsDir   string
	DatabaseMaxConn int32
	AuthRate        int
	AuthBurst       int
	TokenTTL        time.Duration
	ShutdownTimeout time.Duration
}

func Load() (Config, error) {
	maxConnections, err := intFromEnvironment("DB_MAX_CONNECTIONS", 16)
	if err != nil {
		return Config{}, err
	}
	authRate, err := intFromEnvironment("AUTH_RATE_PER_SECOND", 5)
	if err != nil {
		return Config{}, err
	}
	authBurst, err := intFromEnvironment("AUTH_RATE_BURST", 20)
	if err != nil {
		return Config{}, err
	}

	shutdownTimeout, err := durationFromEnvironment("SHUTDOWN_TIMEOUT", 10*time.Second)
	if err != nil {
		return Config{}, err
	}

	tokenTTL, err := durationFromEnvironment("TOKEN_TTL", 24*time.Hour)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Address:         httpAddress(),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		JWTSecret:       os.Getenv("JWT_SECRET"),
		AdminEmail:      os.Getenv("ADMIN_EMAIL"),
		AdminPassword:   os.Getenv("ADMIN_PASSWORD"),
		MigrationsDir:   stringFromEnvironment("MIGRATIONS_DIR", "migrations"),
		DatabaseMaxConn: int32(maxConnections),
		AuthRate:        authRate,
		AuthBurst:       authBurst,
		TokenTTL:        tokenTTL,
		ShutdownTimeout: shutdownTimeout,
	}

	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	if len(cfg.JWTSecret) < 32 {
		return Config{}, errors.New("JWT_SECRET must contain at least 32 characters")
	}
	if cfg.AdminEmail == "" || cfg.AdminPassword == "" {
		return Config{}, errors.New("ADMIN_EMAIL and ADMIN_PASSWORD are required")
	}

	if cfg.DatabaseMaxConn < 1 {
		return Config{}, errors.New("DB_MAX_CONNECTIONS must be positive")
	}
	if cfg.AuthRate < 1 || cfg.AuthBurst < 1 {
		return Config{}, errors.New("AUTH_RATE_PER_SECOND and AUTH_RATE_BURST must be positive")
	}

	return cfg, nil
}

func httpAddress() string {
	if address := os.Getenv("HTTP_ADDRESS"); address != "" {
		return address
	}
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":8080"
}

func stringFromEnvironment(key string, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func intFromEnvironment(key string, fallback int) (int, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}

	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}

	return parsed, nil
}

func durationFromEnvironment(key string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}

	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", key, err)
	}

	return parsed, nil
}
