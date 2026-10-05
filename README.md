# Seat Reservation Service

## Live deployment

Base URL: `https://api-production-45a5.up.railway.app`

Demo administrator credentials for this disposable deployment:

```text
Email: admin@seat-reservation.demo
Password: DemoAdmin-2026!
```

These credentials are public and must not be reused outside this demonstration.

`ADMIN_EMAIL` and `ADMIN_PASSWORD` only create the bootstrap administrator when that email does not exist. Restarting with a different password does not reset the existing account or change its role.

## Burst test

```bash
export ADMIN_EMAIL='admin@seat-reservation.demo'
export ADMIN_PASSWORD='DemoAdmin-2026!'
export METRICS_BEARER_TOKEN='<metrics token, if configured>'
./burst.sh https://api-production-45a5.up.railway.app
```

`METRICS_BEARER_TOKEN` is required when the target protects `/metrics`.

The default run sends 20,000 mixed single-seat and multi-seat reservation requests with curl's parallel transfer engine. It also verifies idempotent replay and conflict behavior, reports confirmations and declines, checks duplicate ownership, all-or-nothing behavior, and the per-user limit, then reconciles API seat state with Prometheus gauges. Gauge comparison waits six seconds so any five-second collector cache entry has expired.

## Metrics

Prometheus metrics are exposed at `/metrics`. Set `METRICS_BEARER_TOKEN` to protect the endpoint for a hosted scraper.

Public Grafana dashboard: `https://robustturret3544.grafana.net/public-dashboards/fad3834ab4e9410381c30d957e45401a`

Grafana Cloud scrapes and stores the production metrics once per minute. The raw endpoint requires the deployment's bearer token:

```bash
curl -H "Authorization: Bearer $METRICS_BEARER_TOKEN" \
  https://api-production-45a5.up.railway.app/metrics
```

Reservation outcome and HTTP request counters are process-local and reset normally when an instance restarts. Prometheus retains previously scraped samples, so historical storage and restart-aware queries provide continuity rather than the application persisting counter values. With multiple instances, scrape every instance and aggregate their series.

Per-show available, held, confirmed, and total seat gauges are read from PostgreSQL. Each service instance caches a successful collection for five seconds, so a scrape can trail `GET /shows/{id}` by up to five seconds. A collection failure serves the previous snapshot and increments `seat_reservation_metrics_collection_errors_total`.

The per-show collector is intentionally useful for demonstration and reconciliation, but each refresh groups the full seats table and emits four series for every show. Query cost and `show_id` label cardinality therefore grow with retained shows, and every service instance maintains its own cache and runs its own refresh. A production deployment with unbounded show history should limit the collector to active shows or use recording/export pipelines appropriate to its retention and scale requirements.

## Logs and trace correlation

The API writes structured JSON logs to standard output with `service`, `event`, `requestId`, `traceId`, and `spanId` fields. Every response includes `X-Request-ID`, `X-Trace-ID`, and a W3C `Traceparent` header. A valid incoming `X-Request-ID` and `Traceparent` trace ID are preserved, allowing one request to be followed across callers, API request logs, and reservation domain events.

Set `LOKI_PUSH_URL`, `LOKI_USERNAME`, and `LOKI_PASSWORD` to broadcast the same JSON records to Grafana Cloud Loki in compressed batches. The asynchronous queue defaults to 16 MiB and is capped at 64 MiB, preventing a Loki outage from exhausting application memory. Queue overflow, delivery failures, queued bytes, and delivered records are exposed as Prometheus metrics.

## Public observability

Logs, metrics, and traces are all readable without a login on the public dashboard: `https://robustturret3544.grafana.net/public-dashboards/fad3834ab4e9410381c30d957e45401a`

Every request produces one log line and one trace, joined by `traceId`. Health probes and `/metrics` scrapes are not logged or traced unless they fail.

Loki stream labels are `service_name`, `environment`, `source`, and `level`. Everything else is a JSON field:

```logql
{service_name="seat-reservation"} | json | event="reservation_declined" | reason="seats_unavailable"
{service_name="seat-reservation", level="error"}
{service_name="seat-reservation"} | json | requestId="<id from X-Request-ID>"
```

Each request trace is a complete tree: HTTP request, authentication, the service operation, and each reservation step (`lock_user_show`, `check_idempotency`, `check_limit`, `lock_seats`, `insert_reservation`, `confirm_seats`, `commit`), with every database call and the connection-pool wait as child spans. Domain declines carry `app.outcome=declined` and `app.decline_reason` on the step that refused; unexpected failures set the span status to error with the recorded exception. Search in Tempo:

```traceql
{ resource.service.name="seat-reservation" && span.app.outcome="reservation_declined" }
{ resource.service.name="seat-reservation" && span.app.reason="seat_limit_exceeded" }
{ resource.service.name="seat-reservation" && status=error }
{ resource.service.name="seat-reservation" && span.app.user_id="<user id>" }
```

Set `OTLP_ENDPOINT`, `OTLP_USERNAME`, and `OTLP_PASSWORD` to export traces. Remote endpoints must use HTTPS. A reservation burst emits roughly twenty spans per request, so spans are exported by `OTLP_EXPORT_WORKERS` parallel workers (default 8) from a queue of `OTLP_QUEUE_SPANS` spans (default 131072). `OTLP_SAMPLE_RATIO` (default 1) keeps whole traces when lowered. `seat_reservation_trace_spans_ended_total` minus `seat_reservation_trace_spans_exported_total` shows spans still queued or lost, and `seat_reservation_log_broadcast_queue_dropped_total` shows lost log lines.

## Reservation idempotency

`POST /shows/{show_id}/reserve` scopes each idempotency key to the authenticated user.

- Repeating the same key with the same show and seats returns the original reservation.
- Reusing the key with a different show or different seats returns `409 idempotency_conflict`.
- If the original reservation was cancelled, replaying its key returns that original reservation with `status: cancelled`. It does not create a new reservation or confirm the released seats.
- Reserving again after cancellation requires a new idempotency key.
