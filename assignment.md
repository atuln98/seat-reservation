# TAKE-HOME EXERCISE — Seat Reservation at Scale

Deploy & Observe round · Backend Engineering, Paytm Money

Time budget: approximately one day

AI tools are allowed and expected. Disclose how they were used.

## Short version

Build, deploy, and operate a small service that sells assigned seats for an event and lets users reserve them.

Correctness must hold under load:

- Never sell the same seat twice.
- Never let a user exceed their booking limit.
- Never double-charge or create another reservation for a retried request.
- Preserve correctness when thousands of buyers hit the same show simultaneously.
- Make the running system observable in real time.

The running service is graded more heavily than the write-up.

## Scenario

A show with `N` numbered seats opens at `t=0`. Tens of thousands of buyers may request seats within the same second, including hundreds of buyers targeting the same hot seat.

Exactly one request may win a contested seat. Every loser must receive a clean domain decline, not an internal server error.

## Functional requirements

### 1. Create a show

```http
POST /shows
```

Admin only.

```json
{
  "name": "friday-night",
  "seats": ["A1", "A2", "A3"],
  "price_paise": 25000
}
```

Return the created show with an ID and every seat in the `available` state.

Money values are integer minor units and must never use floating-point numbers.

### 2. Reserve seats

```http
POST /shows/{id}/reserve
```

Authenticated user. Identity must come from the authentication token, not the request body.

```json
{
  "seats": ["A12"],
  "idempotency_key": "..."
}
```

Successful response:

```json
{
  "reservation_id": "...",
  "show_id": "...",
  "user_id": "...",
  "seats": ["A12"],
  "amount_paise": 25000,
  "status": "confirmed"
}
```

Required behaviour:

- A confirmed or actively held seat can never be confirmed for another user.
- A race for one seat produces exactly one winner; losers receive `409`.
- A user cannot hold more than `per_user_limit` seats for one show. The default limit is four.
- Reusing an idempotency key with the same request returns the original reservation without creating another reservation.
- Reusing an idempotency key with a different set of seats returns `409`.
- Multi-seat behaviour must be documented as all-or-nothing or best-effort and remain correct under concurrency.

### 3. Release or expire

Choose one model:

- `POST /reservations/{id}/cancel`, where only the owner can cancel.
- A time-boxed hold that expires automatically and makes its seats available again.

A released seat must be safely rebookable. A release must never resurrect or release a seat already confirmed to somebody else.

### 4. Show state

```http
GET /shows/{id}
```

Return every seat's state and aggregate counts.

The following invariant must always hold:

```text
available + held + confirmed = total_seats
```

### 5. Health and metrics

Implement the health and observability requirements below.

## Correctness bar

Assume approximately 20,000 concurrent reservation requests against a fresh show. Requests may target the same hot seats and may retry with the same idempotency key.

The service must satisfy all of the following:

1. No seat is ever confirmed to two users. Each contested seat produces exactly one `201`; all other requests receive `409`.
2. The burst produces zero `5xx` responses. Expected declines are domain-level `4xx` responses.
3. `available + held + confirmed = total_seats` holds during and after the burst.
4. Idempotent retries create no additional reservations. The same key with different seats receives `409`.
5. A user sending ten parallel reservations against a limit-four show ends with at most four active seats.
6. Identity is derived from the token. A spoofed body field cannot act as another user, and only a reservation owner can cancel it.

The atomic decision must live in the datastore. Valid mechanisms include:

- A conditional update guarded by current state.
- A unique constraint that prevents duplicate ownership.
- Deterministically ordered row locks for multi-seat requests.

A read followed by an unguarded write is not safe.

## Deploy and observe

### Deployment

- Deploy to a public URL using Render, Railway, Fly.io, or a similar platform.
- The service must survive a cold start and become healthy.
- Include a Dockerfile and Compose configuration so a clean checkout runs consistently.

### Health

- Expose a liveness endpoint.
- Expose a readiness endpoint that checks database reachability and fails closed when the database is unavailable.

### Metrics

Expose Prometheus-style metrics including at least:

- Reservations confirmed.
- Reservations declined by reason:
  - seat taken;
  - per-user limit;
  - idempotent replay.
- Seats available.

Metrics must reconcile with API state and observed behaviour.

### Logs

- Emit structured logs.
- Include a correlation or request ID.
- Provide public log access when supported, or a short recording of logs during a burst.

### Burst test

Include a one-command load test such as:

```bash
make burst
```

or:

```bash
./burst.sh <BASE_URL>
```

The burst must:

- Target the live URL.
- Include many users targeting the same hot seat.
- Print confirmed requests.
- Print declines grouped by reason.
- Print `5xx` responses.
- Print final reconciliation counts.

## Deliverables

1. Public Git repository with incremental commit history.
2. Live deployed URL.
3. One-command burst script documented in the README.
4. Metrics and logs access.
5. `WRITEUP.md` covering:
   - the exact atomic mechanism and why it is race-free;
   - deterministic ordering and multi-seat deadlock avoidance;
   - idempotency storage and same-key/different-body handling;
   - cancellation or hold expiry;
   - consistency versus availability during a partition;
   - observability and what should page an operator;
   - specific and honest AI usage;
   - future improvements.

## Ground rules

- Use integer paise for money.
- AI tools may be used, but usage must be disclosed honestly.
- Be prepared to extend and explain the system during a live interview.
- A clean checkout must build and run.
- The service must be deployed and reachable when reviewed.
- A JSON API is sufficient; no UI is required.
