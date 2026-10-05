# Seat Reservation Service

## Live deployment

Base URL: `https://api-production-45a5.up.railway.app`

Demo administrator credentials for this disposable deployment:

```text
Email: admin@seat-reservation.demo
Password: DemoAdmin-2026!
```

These credentials are public and must not be reused outside this demonstration.

## Burst test

```bash
ADMIN_EMAIL=admin@seat-reservation.demo \
ADMIN_PASSWORD='DemoAdmin-2026!' \
./burst.sh https://api-production-45a5.up.railway.app
```

The default run sends 20,000 mixed single-seat and multi-seat reservation requests with curl's parallel transfer engine. It reports confirmations, declines grouped by reason, server and client failures, duplicate ownership, all-or-nothing behavior, the per-user limit scenario, and final seat reconciliation.

## Metrics

Prometheus metrics are exposed at `/metrics`. Set `METRICS_BEARER_TOKEN` to protect the endpoint for a hosted scraper. Reservation outcomes and HTTP request metrics are held in process memory. Per-show seat gauges are read from PostgreSQL and cached for five seconds, so available, held, confirmed, and total values reconcile with `GET /shows/{id}` without querying the database on every scrape.

```bash
curl -H "Authorization: Bearer $METRICS_BEARER_TOKEN" \
  https://api-production-45a5.up.railway.app/metrics
```

## Logs and trace correlation

The API writes structured JSON logs to standard output with `service`, `event`, `requestId`, `traceId`, and `spanId` fields. Every response includes `X-Request-ID`, `X-Trace-ID`, and a W3C `Traceparent` header. A valid incoming `X-Request-ID` and `Traceparent` trace ID are preserved, allowing one request to be followed across callers, API request logs, and reservation domain events.

## Reservation idempotency

`POST /shows/{show_id}/reserve` scopes each idempotency key to the authenticated user.

- Repeating the same key with the same show and seats returns the original reservation.
- Reusing the key with a different show or different seats returns `409 idempotency_conflict`.
- If the original reservation was cancelled, replaying its key returns that original reservation with `status: cancelled`. It does not create a new reservation or confirm the released seats.
- Reserving again after cancellation requires a new idempotency key.
