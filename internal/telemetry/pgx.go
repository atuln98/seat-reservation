package telemetry

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
)

type PGXTracer struct{}

func NewPGXTracer() PGXTracer {
	return PGXTracer{}
}

func (PGXTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !oteltrace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	statement := strings.Join(strings.Fields(data.SQL), " ")
	operation := "query"
	if first, _, _ := strings.Cut(statement, " "); first != "" {
		operation = strings.ToUpper(first)
	}
	if operation == "BEGIN" || operation == "ROLLBACK" {
		return ctx
	}
	if len(statement) > 240 {
		statement = statement[:240]
	}
	ctx, _ = Start(
		ctx,
		"db "+operation,
		attribute.String("db.system.name", "postgresql"),
		attribute.String("db.operation.name", operation),
		attribute.String("db.query.text", statement),
	)
	return ctx
}

func (PGXTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span := oteltrace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.SetAttributes(attribute.Int64("db.rows_affected", data.CommandTag.RowsAffected()))
	if data.Err != nil {
		var postgres *pgconn.PgError
		if errors.As(data.Err, &postgres) {
			span.SetAttributes(
				attribute.String("db.response.status_code", postgres.Code),
				attribute.String("db.constraint", postgres.ConstraintName),
			)
		}
	}
	Finish(span, data.Err)
}

type acquireStartKey struct{}

const slowAcquire = 2 * time.Millisecond

func (PGXTracer) TraceAcquireStart(ctx context.Context, pool *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	if !oteltrace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	stat := pool.Stat()
	return context.WithValue(ctx, acquireStartKey{}, acquireStart{
		at:       time.Now(),
		acquired: int(stat.AcquiredConns()),
		idle:     int(stat.IdleConns()),
		maximum:  int(stat.MaxConns()),
	})
}

type acquireStart struct {
	at       time.Time
	acquired int
	idle     int
	maximum  int
}

func (PGXTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	started, ok := ctx.Value(acquireStartKey{}).(acquireStart)
	if !ok {
		return
	}
	waited := time.Since(started.at)
	if waited < slowAcquire && data.Err == nil {
		return
	}
	_, span := otel.Tracer(tracerName).Start(
		ctx,
		"db pool.acquire",
		oteltrace.WithTimestamp(started.at),
		oteltrace.WithAttributes(
			attribute.String("db.system.name", "postgresql"),
			attribute.Int("db.pool.acquired", started.acquired),
			attribute.Int("db.pool.idle", started.idle),
			attribute.Int("db.pool.max", started.maximum),
			attribute.Int64("db.pool.wait_ms", waited.Milliseconds()),
		),
	)
	Finish(span, data.Err)
}
