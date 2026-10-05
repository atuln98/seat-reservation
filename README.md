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

`METRICS_BEARER_TOKEN` is required: the script reads `/metrics` before and after the run to reconcile the counters, and fails immediately if it cannot. `BURST_USERS` must be at least 27.

The default run sends 20,000 mixed single-seat and multi-seat reservation requests with curl's parallel transfer engine and fails (`result=fail`, non-zero exit) if any check does not hold. It verifies:

- **No double sale:** exactly one 201 per contested seat, no duplicate ownership, and every other request is a clean 409.
- **Zero 5xx** and no client errors or timeouts.
- **Invariant during and after the burst:** `available + held + confirmed = total` is sampled about every 200 ms while the burst runs, and again at the end.
- **Multi-seat behaviour:** requests are all-or-nothing, overlapping requests run concurrently without deadlock, and a failed request leaves its other seat available.
- **Idempotency:** a sequential retry returns the original reservation, the same key with different seats returns 409, 60 concurrent requests with one key create exactly one reservation, and 40 concurrent requests with one key and two different seats create exactly one.
- **Per-user limit:** 10 parallel reservations against a limit of 4 confirm exactly 4.
- **Identity:** missing and invalid tokens return 401, a spoofed `user_id` in the body is rejected with 400 and reserves nothing, the reservation belongs to the token's user, and another user cannot cancel it (404).
- **Cancellation:** a cancel racing 24 competing reservations never releases or resurrects a seat confirmed to someone else, and the seat stays rebookable.
- **Metrics:** the seat gauges match `GET /shows/{id}`, and the confirmed, seat-taken, per-user-limit, idempotency-conflict and idempotent-replay counters match what the script observed. This assumes no other client is using the service during the run.

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

Logs, metrics, and request spans are all readable without a login on the public dashboard: `https://robustturret3544.grafana.net/public-dashboards/fad3834ab4e9410381c30d957e45401a`

Every request produces one log line, and the inside of the request is written to Loki as span logs (`event="span"`): the HTTP request, authentication, each reservation step (`lock_user_show`, `check_idempotency`, `check_limit`, `lock_seats`, `insert_reservation`, `confirm_seats`, `commit`), the connection-pool wait when it is 2 ms or more, and every database call. Spans of one request share a `traceId`, are indented by depth, and are listed root first. Every confirmation, cancellation, 5xx and unexpected failure is logged; declines and other routine requests are sampled at 10 per second per reason. Health probes and `/metrics` scrapes are not logged unless they fail.

Grafana Cloud's Tempo trace panels do not run for anonymous visitors (Tempo search streams over a login-only channel), so the public dashboard reads spans from these span logs. Full trace trees are still exported to Tempo for logged-in users.

Loki stream labels are `service_name`, `environment`, `source`, and `level`. Everything else is a JSON field:

```logql
{service_name="seat-reservation"} | json | event="span" | traceId="<trace id>"
{service_name="seat-reservation"} | json | event="span" | spanStatus="error"
{service_name="seat-reservation"} | json | outcome="reservation_declined" | reason="seats_unavailable"
{service_name="seat-reservation", level="error"}
{service_name="seat-reservation"} | json | requestId="<id from X-Request-ID>"
```

Set `OTLP_ENDPOINT`, `OTLP_USERNAME`, and `OTLP_PASSWORD` to export traces to Tempo. Remote endpoints must use HTTPS. Spans are exported by `OTLP_EXPORT_WORKERS` parallel workers (default 8) from a queue of `OTLP_QUEUE_SPANS` spans (default 131072). `OTLP_SAMPLE_RATIO` (default 1) keeps whole traces when lowered and does not affect span logs. `seat_reservation_trace_spans_ended_total` minus `seat_reservation_trace_spans_exported_total` shows spans still queued or lost, and `seat_reservation_log_broadcast_queue_dropped_total` shows lost log lines.

## Reservation idempotency

`POST /shows/{show_id}/reserve` scopes each idempotency key to the authenticated user.

- Repeating the same key with the same show and seats returns the original reservation.
- Reusing the key with a different show or different seats returns `409 idempotency_conflict`.
- If the original reservation was cancelled, replaying its key returns that original reservation with `status: cancelled`. It does not create a new reservation or confirm the released seats.
- Reserving again after cancellation requires a new idempotency key.
