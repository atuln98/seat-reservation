package telemetry

import (
	"context"
	"errors"
	"testing"

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
