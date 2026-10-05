package telemetry

import (
	"context"
	"encoding/base64"
	"errors"
	"net/netip"
	"net/url"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

type Config struct {
	Endpoint       string
	Username       string
	Password       string
	Environment    string
	RequestTimeout time.Duration
	SampleRatio    float64
}

type Stats struct {
	ended          atomic.Uint64
	exported       atomic.Uint64
	exportErrors   atomic.Uint64
	exportFailures atomic.Uint64
}

func (stats *Stats) EndedTotal() uint64 {
	return stats.ended.Load()
}

func (stats *Stats) ExportedTotal() uint64 {
	return stats.exported.Load()
}

func (stats *Stats) ExportErrorsTotal() uint64 {
	return stats.exportErrors.Load()
}

func (stats *Stats) ExportFailedSpansTotal() uint64 {
	return stats.exportFailures.Load()
}

type countingProcessor struct {
	stats *Stats
}

func (processor countingProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

func (processor countingProcessor) OnEnd(sdktrace.ReadOnlySpan) {
	processor.stats.ended.Add(1)
}

func (processor countingProcessor) Shutdown(context.Context) error {
	return nil
}

func (processor countingProcessor) ForceFlush(context.Context) error {
	return nil
}

type countingExporter struct {
	inner sdktrace.SpanExporter
	stats *Stats
}

func (exporter countingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := exporter.inner.ExportSpans(ctx, spans)
	if err != nil {
		exporter.stats.exportErrors.Add(1)
		exporter.stats.exportFailures.Add(uint64(len(spans)))
		return err
	}
	exporter.stats.exported.Add(uint64(len(spans)))
	return nil
}

func (exporter countingExporter) Shutdown(ctx context.Context) error {
	return exporter.inner.Shutdown(ctx)
}

type routeSampler struct {
	inner sdktrace.Sampler
}

func (sampler routeSampler) ShouldSample(parameters sdktrace.SamplingParameters) sdktrace.SamplingResult {
	for _, item := range parameters.Attributes {
		if item.Key != "url.path" {
			continue
		}
		switch item.Value.AsString() {
		case "/", "/health/live", "/health/ready", "/metrics":
			return sdktrace.SamplingResult{
				Decision:   sdktrace.Drop,
				Tracestate: oteltrace.SpanContextFromContext(parameters.ParentContext).TraceState(),
			}
		}
	}
	return sampler.inner.ShouldSample(parameters)
}

func (sampler routeSampler) Description() string {
	return "RouteFilter{" + sampler.inner.Description() + "}"
}

func New(ctx context.Context, cfg Config) (*sdktrace.TracerProvider, *Stats, error) {
	configured := cfg.Endpoint != "" || cfg.Username != "" || cfg.Password != ""
	if configured && (cfg.Endpoint == "" || cfg.Username == "" || cfg.Password == "") {
		return nil, nil, errors.New("OTLP_ENDPOINT, OTLP_USERNAME, and OTLP_PASSWORD must be configured together")
	}

	stats := &Stats{}
	serviceResource := resource.NewWithAttributes(
		"",
		attribute.String("service.name", "seat-reservation"),
		attribute.String("deployment.environment.name", cfg.Environment),
	)
	options := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(serviceResource),
		sdktrace.WithSpanProcessor(countingProcessor{stats: stats}),
	}
	if !configured {
		options = append(options, sdktrace.WithSampler(sdktrace.NeverSample()))
		return sdktrace.NewTracerProvider(options...), stats, nil
	}

	if cfg.SampleRatio < 0 || cfg.SampleRatio > 1 {
		return nil, nil, errors.New("OTLP_SAMPLE_RATIO must be between 0 and 1")
	}
	parsed, err := url.Parse(cfg.Endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, nil, errors.New("OTLP_ENDPOINT must be an absolute HTTP or HTTPS URL")
	}
	if parsed.Scheme != "https" && !loopbackHost(parsed.Hostname()) {
		return nil, nil, errors.New("OTLP_ENDPOINT must use HTTPS unless it targets a loopback host")
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
		return nil, nil, err
	}

	options = append(
		options,
		sdktrace.WithSampler(sdktrace.ParentBased(routeSampler{inner: sdktrace.TraceIDRatioBased(cfg.SampleRatio)})),
		sdktrace.WithBatcher(
			countingExporter{inner: exporter, stats: stats},
			sdktrace.WithMaxQueueSize(65536),
			sdktrace.WithMaxExportBatchSize(1024),
			sdktrace.WithBatchTimeout(time.Second),
			sdktrace.WithExportTimeout(cfg.RequestTimeout),
		),
	)
	return sdktrace.NewTracerProvider(options...), stats, nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.IsLoopback()
}
