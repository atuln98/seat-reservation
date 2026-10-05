package telemetry

import (
	"context"
	"encoding/base64"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type Config struct {
	Endpoint       string
	Username       string
	Password       string
	Environment    string
	RequestTimeout time.Duration
}

func New(ctx context.Context, cfg Config) (*sdktrace.TracerProvider, error) {
	configured := cfg.Endpoint != "" || cfg.Username != "" || cfg.Password != ""
	if configured && (cfg.Endpoint == "" || cfg.Username == "" || cfg.Password == "") {
		return nil, errors.New("OTLP_ENDPOINT, OTLP_USERNAME, and OTLP_PASSWORD must be configured together")
	}

	serviceResource := resource.NewWithAttributes(
		"",
		attribute.String("service.name", "seat-reservation"),
		attribute.String("deployment.environment.name", cfg.Environment),
	)
	options := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(serviceResource),
	}
	if !configured {
		options = append(options, sdktrace.WithSampler(sdktrace.NeverSample()))
		return sdktrace.NewTracerProvider(options...), nil
	}

	authorization := base64.StdEncoding.EncodeToString([]byte(cfg.Username + ":" + cfg.Password))
	exporter, err := otlptracehttp.New(
		ctx,
		otlptracehttp.WithEndpointURL(cfg.Endpoint),
		otlptracehttp.WithHeaders(map[string]string{"Authorization": "Basic " + authorization}),
		otlptracehttp.WithCompression(otlptracehttp.GzipCompression),
		otlptracehttp.WithTimeout(cfg.RequestTimeout),
	)
	if err != nil {
		return nil, err
	}

	options = append(
		options,
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithBatcher(
			exporter,
			sdktrace.WithMaxQueueSize(16384),
			sdktrace.WithMaxExportBatchSize(512),
			sdktrace.WithBatchTimeout(time.Second),
			sdktrace.WithExportTimeout(cfg.RequestTimeout),
		),
	)
	return sdktrace.NewTracerProvider(options...), nil
}
