package telemetry

import (
	"context"
	"encoding/base64"
	"errors"
	"net/netip"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
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
	ExportWorkers  int
	QueueSpans     int
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

func (processor countingProcessor) OnEnd(span sdktrace.ReadOnlySpan) {
	if span.SpanContext().IsSampled() {
		processor.stats.ended.Add(1)
	}
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

type parallelExporter struct {
	inner   sdktrace.SpanExporter
	timeout time.Duration
	batches chan []sdktrace.ReadOnlySpan
	workers sync.WaitGroup
}

func newParallelExporter(inner sdktrace.SpanExporter, workers int, buffer int, timeout time.Duration) *parallelExporter {
	exporter := &parallelExporter{
		inner:   inner,
		timeout: timeout,
		batches: make(chan []sdktrace.ReadOnlySpan, buffer),
	}
	for range workers {
		exporter.workers.Add(1)
		go func() {
			defer exporter.workers.Done()
			for batch := range exporter.batches {
				ctx, cancel := context.WithTimeout(context.Background(), exporter.timeout)
				if err := exporter.inner.ExportSpans(ctx, batch); err != nil {
					otel.Handle(err)
				}
				cancel()
			}
		}()
	}
	return exporter
}

func (exporter *parallelExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	batch := append([]sdktrace.ReadOnlySpan(nil), spans...)
	select {
	case exporter.batches <- batch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (exporter *parallelExporter) Shutdown(ctx context.Context) error {
	close(exporter.batches)
	done := make(chan struct{})
	go func() {
		exporter.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
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
	result := sampler.inner.ShouldSample(parameters)
	if result.Decision == sdktrace.Drop {
		result.Decision = sdktrace.RecordOnly
	}
	return result
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
		sdktrace.WithSpanProcessor(DefaultCollector),
	}
	if !configured {
		options = append(options, sdktrace.WithSampler(routeSampler{inner: sdktrace.NeverSample()}))
		return sdktrace.NewTracerProvider(options...), stats, nil
	}

	if cfg.SampleRatio < 0 || cfg.SampleRatio > 1 {
		return nil, nil, errors.New("OTLP_SAMPLE_RATIO must be between 0 and 1")
	}
	if cfg.ExportWorkers < 1 || cfg.QueueSpans < 1 {
		return nil, nil, errors.New("OTLP_EXPORT_WORKERS and OTLP_QUEUE_SPANS must be positive")
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
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{
			Enabled:         true,
			InitialInterval: 500 * time.Millisecond,
			MaxInterval:     5 * time.Second,
			MaxElapsedTime:  time.Minute,
		}),
	)
	if err != nil {
		return nil, nil, err
	}

	options = append(
		options,
		sdktrace.WithSampler(routeSampler{inner: sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))}),
		sdktrace.WithBatcher(
			newParallelExporter(
				countingExporter{inner: exporter, stats: stats},
				cfg.ExportWorkers,
				cfg.ExportWorkers*4,
				time.Minute,
			),
			sdktrace.WithMaxQueueSize(cfg.QueueSpans),
			sdktrace.WithMaxExportBatchSize(1024),
			sdktrace.WithBatchTimeout(500*time.Millisecond),
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
