package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Address            string
	DatabaseURL        string
	JWTSecret          string
	AdminEmail         string
	AdminPassword      string
	MigrationsDir      string
	DatabaseMaxConn    int32
	AuthRate           int
	AuthBurst          int
	MetricsBearerToken string
	LokiPushURL        string
	LokiUsername       string
	LokiPassword       string
	LokiEnvironment    string
	LokiQueueBytes     int
	LokiBatchBytes     int
	OTLPEndpoint       string
	OTLPUsername       string
	OTLPPassword       string
	OTLPSampleRatio    float64
	TokenTTL           time.Duration
	LokiFlushInterval  time.Duration
	LokiRequestTimeout time.Duration
	OTLPRequestTimeout time.Duration
	RequestTimeout     time.Duration
	ShutdownTimeout    time.Duration
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
	lokiQueueBytes, err := intFromEnvironment("LOKI_QUEUE_BYTES", 16*1024*1024)
	if err != nil {
		return Config{}, err
	}
	lokiBatchBytes, err := intFromEnvironment("LOKI_BATCH_BYTES", 512*1024)
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
	requestTimeout, err := durationFromEnvironment("REQUEST_TIMEOUT", 10*time.Second)
	if err != nil {
		return Config{}, err
	}
	lokiFlushInterval, err := durationFromEnvironment("LOKI_FLUSH_INTERVAL", 250*time.Millisecond)
	if err != nil {
		return Config{}, err
	}
	lokiRequestTimeout, err := durationFromEnvironment("LOKI_REQUEST_TIMEOUT", 5*time.Second)
	if err != nil {
		return Config{}, err
	}
	otlpSampleRatio, err := floatFromEnvironment("OTLP_SAMPLE_RATIO", 1)
	if err != nil {
		return Config{}, err
	}
	otlpRequestTimeout, err := durationFromEnvironment("OTLP_REQUEST_TIMEOUT", 5*time.Second)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Address:            httpAddress(),
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		JWTSecret:          os.Getenv("JWT_SECRET"),
		AdminEmail:         os.Getenv("ADMIN_EMAIL"),
		AdminPassword:      os.Getenv("ADMIN_PASSWORD"),
		MigrationsDir:      stringFromEnvironment("MIGRATIONS_DIR", "migrations"),
		DatabaseMaxConn:    int32(maxConnections),
		AuthRate:           authRate,
		AuthBurst:          authBurst,
		MetricsBearerToken: os.Getenv("METRICS_BEARER_TOKEN"),
		LokiPushURL:        os.Getenv("LOKI_PUSH_URL"),
		LokiUsername:       os.Getenv("LOKI_USERNAME"),
		LokiPassword:       os.Getenv("LOKI_PASSWORD"),
		LokiEnvironment:    stringFromEnvironment("LOKI_ENVIRONMENT", stringFromEnvironment("RAILWAY_ENVIRONMENT_NAME", "production")),
		LokiQueueBytes:     lokiQueueBytes,
		LokiBatchBytes:     lokiBatchBytes,
		OTLPEndpoint:       os.Getenv("OTLP_ENDPOINT"),
		OTLPUsername:       os.Getenv("OTLP_USERNAME"),
		OTLPPassword:       os.Getenv("OTLP_PASSWORD"),
		OTLPSampleRatio:    otlpSampleRatio,
		TokenTTL:           tokenTTL,
		LokiFlushInterval:  lokiFlushInterval,
		LokiRequestTimeout: lokiRequestTimeout,
		OTLPRequestTimeout: otlpRequestTimeout,
		RequestTimeout:     requestTimeout,
		ShutdownTimeout:    shutdownTimeout,
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
	if cfg.MetricsBearerToken != "" && len(cfg.MetricsBearerToken) < 32 {
		return Config{}, errors.New("METRICS_BEARER_TOKEN must contain at least 32 characters")
	}
	lokiConfigured := cfg.LokiPushURL != "" || cfg.LokiUsername != "" || cfg.LokiPassword != ""
	if lokiConfigured && (cfg.LokiPushURL == "" || cfg.LokiUsername == "" || cfg.LokiPassword == "") {
		return Config{}, errors.New("LOKI_PUSH_URL, LOKI_USERNAME, and LOKI_PASSWORD must be configured together")
	}
	if cfg.LokiQueueBytes < 1 || cfg.LokiQueueBytes > 64*1024*1024 {
		return Config{}, errors.New("LOKI_QUEUE_BYTES must be between 1 and 67108864")
	}
	if cfg.LokiBatchBytes < 1 || cfg.LokiBatchBytes > cfg.LokiQueueBytes {
		return Config{}, errors.New("LOKI_BATCH_BYTES must be positive and no greater than LOKI_QUEUE_BYTES")
	}
	if cfg.LokiFlushInterval <= 0 || cfg.LokiRequestTimeout <= 0 {
		return Config{}, errors.New("LOKI_FLUSH_INTERVAL and LOKI_REQUEST_TIMEOUT must be positive")
	}
	if cfg.OTLPSampleRatio < 0 || cfg.OTLPSampleRatio > 1 {
		return Config{}, errors.New("OTLP_SAMPLE_RATIO must be between 0 and 1")
	}
	if cfg.OTLPRequestTimeout <= 0 {
		return Config{}, errors.New("OTLP_REQUEST_TIMEOUT must be positive")
	}
	if cfg.RequestTimeout <= 0 {
		return Config{}, errors.New("REQUEST_TIMEOUT must be positive")
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

func floatFromEnvironment(key string, fallback float64) (float64, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}

	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number: %w", key, err)
	}

	return parsed, nil
}
