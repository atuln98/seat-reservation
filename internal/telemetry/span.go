package telemetry

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const tracerName = "seat-reservation"

func Start(ctx context.Context, name string, attributes ...attribute.KeyValue) (context.Context, oteltrace.Span) {
	return otel.Tracer(tracerName).Start(ctx, name, oteltrace.WithAttributes(attributes...))
}

func Finish(span oteltrace.Span, err error, expected ...error) {
	defer span.End()
	if err == nil {
		return
	}
	for _, domainError := range expected {
		if errors.Is(err, domainError) {
			span.SetAttributes(
				attribute.String("app.outcome", "declined"),
				attribute.String("app.decline_reason", domainError.Error()),
			)
			span.AddEvent("declined", oteltrace.WithAttributes(attribute.String("reason", domainError.Error())))
			return
		}
	}
	RecordFailure(span, err)
}

func RecordFailure(span oteltrace.Span, err error) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		span.SetAttributes(attribute.String("error.type", "deadline_exceeded"))
	case errors.Is(err, context.Canceled):
		span.SetAttributes(attribute.String("error.type", "context_canceled"))
	default:
		span.SetAttributes(attribute.String("error.type", errorType(err)))
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

func errorType(err error) string {
	var postgres interface{ SQLState() string }
	if errors.As(err, &postgres) {
		return "postgres_" + postgres.SQLState()
	}
	return "internal"
}
