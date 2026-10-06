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

`METRICS_BEARER_TOKEN` is required on the live service: the script reads `/metrics` before and after the run to reconcile the counters, and fails immediately if it cannot. Locally (`docker compose up`) `/metrics` is open and the admin is `admin@local.test` / `admin12345`.

Everything runs against one fresh show with a per-user limit of 4. The script registers 60 users (`BURST_USERS`, at least 60), then sends 20,000 requests (`BURST_REQUESTS`) with all of them in flight at once (`BURST_CONCURRENCY`, default equal to the request count). A single curl process runs at most 300 transfers at a time, so the requests are split across 80 curl processes of 250 each. All 80 wait at a start gate and are released together. Against Railway the burst takes about 10 seconds and the whole run about 70.

Railway's edge limits each client IP to roughly 10,000 requests per second, so part of a 20,000-request spike from one machine is turned away with a plain-text 429 or 503 before it reaches the service. The script tells these apart from the service's own responses (which are always JSON), counts them as `rejected_by_railway_edge`, and retries them, together with the deliberately aborted requests, using the same idempotency key. Some of them may have reached the service before the edge gave up, so the retry must return the original reservation rather than book again. That makes these retries an extra idempotency check. A 5xx from the service itself always fails the run.

Locally, Docker on a Mac can't hold 20,000 connections, so use `BURST_CONCURRENCY=1000`. It still sends all 20,000 requests, 1,000 at a time.

The 20,000 requests are shuffled together so that every scenario races every other one:

- **Hot seat:** about 7,500 requests from many users for one seat. Random multi-seat requests also include it.
- **Overlapping multi-seat groups:** `M1,M2` against `M2,M3`, `M4,M5,M6` against `M6,M7`, and `M8,M9,M10`, to exercise the lock ordering. Exactly 3 groups can win.
- **All-or-nothing:** about 900 requests for a free seat together with an already sold one. All must be declined and the free seat must stay available.
- **Random seats:** about 7,500 requests for one to three random seats from a pool of 30, the hot seat and the seats being cancelled. About 4% of them are aborted by the client mid-flight and retried afterwards with the same key.
- **Per-user limit with retries:** 5 users each send 40 parallel requests over 8 seats of their own, every key sent twice. Each must end with exactly 4 seats.
- **Idempotency storms:** 10 keys sent 40 times each at once (exactly one reservation per key, the rest replays with the same ID), and 10 keys sent 40 times alternating between two seats (exactly one reservation, the rest replays or `idempotency_conflict`, and only one of the two seats taken).
- **Cancel races:** 10 owners cancel their reservation 3 times each while, at the same moment, the same key is replayed, another user and an anonymous caller try to cancel it, the owner tries to book the seat again with a new key, and 12 other users try to grab it.
- **Hostile requests:** no token, bad token, spoofed `user_id`, broken JSON, empty, duplicate or control-character seats, missing or 300-character key, 300 seats, an unknown seat with and without the hot seat, a 2 MB body, the wrong content type, and malformed or unknown show and reservation IDs. Each must get its specific 4xx.

Before the burst it checks, one step at a time: a replay returns the original, the same key with another seat gets 409, cancelling is idempotent, replaying a cancelled reservation's key returns it as `cancelled` and books nothing, and identity comes only from the token. After the burst it replays a sample of keys, replays the cancelled ones, cancels them again (which must not free seats that others have since bought), and runs book 2, book 2, get declined, cancel one, book 2, get declined again.

The script fails (`result=fail`, non-zero exit) unless:

- **No double sale:** no seat is won twice, the hot seat is won exactly once, and every confirmed seat in `GET /shows/{id}` belongs to exactly one active reservation according to the receipts the script collected. The seats held by active reservations must equal the API's confirmed seats exactly.
- **Limits and money:** no user holds more than 4 seats, and every reservation is charged `price × seats`.
- **Idempotency:** every key maps to at most one reservation, including keys whose first request was aborted.
- **Zero 5xx** and no client errors apart from the deliberate aborts.
- **Cross-table integrity:** `GET /admin/shows/{id}/integrity` (admin only) compares the `seats` table with the `reservation_seats` and `reservations` tables in one consistent snapshot. Every confirmed seat must have exactly one active owner, no active owner may sit on an unconfirmed seat or a cancelled reservation, and nobody may hold more than the per-user limit. The script calls it about every 200 ms during the burst and again at the end. `available + held + confirmed = total` is checked as well, but it comes from the `seats` table alone and can't fail on its own, so the integrity check and the receipts are the real double-booking guards.
- **Metrics:** the seat gauges match `GET /shows/{id}`, and the confirmed counter equals the number of distinct reservations seen. The decline and replay counters match what the script saw, allowing extra counts only for aborted requests whose server-side outcome the client never received. This assumes no other client is using the service during the run.

It prints confirmed requests, responses grouped by scenario and status, declines grouped by reason, 5xx counts and the final reconciliation.

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
