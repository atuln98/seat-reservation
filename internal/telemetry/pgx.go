package telemetry

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
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
	if len(statement) > 400 {
		statement = statement[:400]
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

func (PGXTracer) TraceAcquireStart(ctx context.Context, pool *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	if !oteltrace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	stat := pool.Stat()
	ctx, _ = Start(
		ctx,
		"db pool.acquire",
		attribute.String("db.system.name", "postgresql"),
		attribute.Int("db.pool.acquired", int(stat.AcquiredConns())),
		attribute.Int("db.pool.idle", int(stat.IdleConns())),
		attribute.Int("db.pool.max", int(stat.MaxConns())),
	)
	return ctx
}

func (PGXTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	span := oteltrace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	Finish(span, data.Err)
}
