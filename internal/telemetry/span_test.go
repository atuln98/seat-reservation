package telemetry

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestFinishDistinguishesDeclinesFromFailures(t *testing.T) {
	declined := errors.New("seat taken")
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	tracer := provider.Tracer("test")

	_, domainSpan := tracer.Start(context.Background(), "domain")
	Finish(domainSpan, declined, declined)
	_, failureSpan := tracer.Start(context.Background(), "failure")
	Finish(failureSpan, errors.New("connection reset"), declined)
	_, successSpan := tracer.Start(context.Background(), "success")
	Finish(successSpan, nil, declined)

	spans := map[string]sdktrace.ReadOnlySpan{}
	for _, span := range recorder.Ended() {
		spans[span.Name()] = span
	}
	if spans["domain"].Status().Code != codes.Unset {
		t.Fatalf("domain decline status = %v", spans["domain"].Status())
	}
	if spans["failure"].Status().Code != codes.Error || len(spans["failure"].Events()) == 0 {
		t.Fatalf("failure was not recorded: %v", spans["failure"].Status())
	}
	if spans["success"].Status().Code != codes.Unset {
		t.Fatalf("success status = %v", spans["success"].Status())
	}
	var reason string
	for _, item := range spans["domain"].Attributes() {
		if item.Key == "app.decline_reason" {
			reason = item.Value.AsString()
		}
	}
	if reason != "seat taken" {
		t.Fatalf("decline reason = %q", reason)
	}
}

func TestRouteSamplerDropsHealthAndMetrics(t *testing.T) {
	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(routeSampler{inner: sdktrace.AlwaysSample()}))
	tracer := provider.Tracer("test")

	_, dropped := tracer.Start(context.Background(), "health", oteltrace.WithAttributes(urlPath("/health/ready")))
	if dropped.IsRecording() {
		t.Fatal("health probe span was sampled")
	}
	_, kept := tracer.Start(context.Background(), "reserve", oteltrace.WithAttributes(urlPath("/shows/1/reserve")))
	if !kept.IsRecording() {
		t.Fatal("reservation span was dropped")
	}
}

func urlPath(path string) attribute.KeyValue {
	return attribute.String("url.path", path)
}

type slowExporter struct {
	delay       time.Duration
	exported    atomic.Int64
	inFlight    atomic.Int64
	maxInFlight atomic.Int64
}

func (exporter *slowExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	current := exporter.inFlight.Add(1)
	for {
		seen := exporter.maxInFlight.Load()
		if current <= seen || exporter.maxInFlight.CompareAndSwap(seen, current) {
			break
		}
	}
	time.Sleep(exporter.delay)
	exporter.exported.Add(int64(len(spans)))
	exporter.inFlight.Add(-1)
	return nil
}

func (exporter *slowExporter) Shutdown(context.Context) error {
	return nil
}

func TestParallelExporterExportsConcurrentlyAndDrainsOnShutdown(t *testing.T) {
	inner := &slowExporter{delay: 50 * time.Millisecond}
	exporter := newParallelExporter(inner, 4, 16, time.Second)

	spans := tracetest.SpanStubs{{Name: "a"}, {Name: "b"}, {Name: "c"}}.Snapshots()
	const batches = 12
	for range batches {
		if err := exporter.ExportSpans(context.Background(), spans); err != nil {
			t.Fatalf("ExportSpans() error = %v", err)
		}
	}
	if err := exporter.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	if got := inner.exported.Load(); got != batches*3 {
		t.Fatalf("exported spans = %d, want %d", got, batches*3)
	}
	if inner.maxInFlight.Load() < 2 {
		t.Fatalf("exports never overlapped, max in flight = %d", inner.maxInFlight.Load())
	}
}
