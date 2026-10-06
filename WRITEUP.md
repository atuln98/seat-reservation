# Seat Reservation Service

This is a small service that sells assigned seats for a show and keeps doing it correctly when thousands of people want the same seat in the same second. The write-up follows one thing at a time: a show, a user, a single reservation, a retry of it, two people fighting over a seat, a cancellation, watching all of it in the logs, what happens when something breaks, and finally the burst that throws 20,000 requests at it at once.

## What I designed and built

The design is mine from top to bottom. I used AI tools (Cursor and Claude Code) the way I'd use a fast pair programmer: to talk ideas through, draft code and docs, and look for holes. I owned every decision, and I reviewed, corrected or refactored every line that went into the repo.

### My decisions

- **Structure:** I laid out the codebase. `cmd/api` is the entry point, and `internal/` is split into `api`, `auth`, `config`, `database`, `httpmiddleware`, `logbroadcast`, `metrics`, `reservation`, `show`, `telemetry` and `user`.
- **Schema:** I designed the tables `users`, `shows`, `seats`, `reservations` and `reservation_seats`, and kept correcting the schema as the design firmed up.
- **Indexes:** I chose them, including `UNIQUE (user_id, idempotency_key)` for idempotency and case-insensitive uniqueness on email.
- **Postgres over Redis:** I chose to do all the locking in Postgres rather than add Redis. Resources on the deployment are limited, and Redis would have been one more moving part to keep consistent with the database. With more room I'd build a Redis version too and compare the two.
- **The locks, and why nothing races:**
  - **No user over the limit:** every reserve and cancel takes a transactional advisory lock on (user, show). A user's requests for one show go through one at a time, so firing parallel requests can't get anyone past the limit. Other users are never held up by it.
  - **No seat sold twice:** seat rows are locked with `SELECT ... FOR UPDATE` until the transaction ends. The first request to get the lock books the seat. Everyone behind it finds the seat already confirmed and gets a clean 409.
  - **No deadlocks:** seats are always locked in seat-number order, and the user lock always comes first.
- **Database-level guard:** I wanted Postgres itself to refuse a double booking, even if the code had a bug or someone edited the tables by hand. That's migration `004_reservation_seat_constraints.sql`:
  - `reservation_seats` carries its own `show_id` and `active` flag, so an index can see them without a join;
  - composite foreign keys tie each seat row to its reservation's show and to a seat that really exists;
  - a unique partial index on `(show_id, seat_number) WHERE active` makes a second active owner impossible.
  - The result is three independent layers: a double booking would have to get past the row locks, the guarded update and this constraint.
- **SQL and query flow:** I decided the order of steps inside the transaction (user lock, idempotency lookup, limit check, seat locks, insert, guarded update, commit) and the query for each step.
- **Business logic:** a multi-seat request is all or nothing. It never books part of a request.
- **Database pool:** one pgx pool of 16 connections per instance. Each booking holds a connection for its whole transaction, so the pool caps how many bookings run at once, and the rest wait instead of overloading Postgres.
- **Auth layer:**
  - identity comes only from the token, never from the request body;
  - the first admin is created from environment variables at startup;
  - admin routes check the role in the database on every request;
  - only the owner can cancel, and everyone else gets a 404.
- **Release model:** explicit cancel instead of time-boxed holds.
- **Observability:** I designed the logging, metrics and tracing layer. That covers what gets logged, request and trace IDs on everything, which metrics exist, a span for every reservation step, and making all of it readable on a public dashboard without a login.
- **How logs flow:**
  - one JSON line per request to stdout;
  - the same line goes through a capped in-memory queue to Grafana Cloud Loki, so a Loki outage drops and counts lines instead of slowing down bookings;
  - spans go to Tempo, and are also written to Loki so the public dashboard can show them;
  - metrics are on `/metrics` for Grafana Cloud to scrape;
  - every line ties back to its request through `requestId` and `traceId`.
- **Testing:**
  - I spotted that `available + held + confirmed = total` can never catch a double booking, and added the integrity endpoint to check ownership across tables;
  - I shaped the burst: everything on one show with overlapping scenarios, every response checked against the receipts the burst collects, and a true 20,000 requests in flight at once;
  - I tested every change on local Docker before deploying it.
- **Problems I found and fixed:**
  - **curl was capping the burst at 300.** I found that the burst was only ever running 300 requests at once, and reworked it to release all 20,000 together.
  - **Railway's per-IP limit.** I worked out that Railway's edge was turning part of the spike away, and made those requests retry with the same key.
  - **A commit that went uncounted.** If a client disconnected mid-commit, the reservation existed but was never counted. I traced the gap between the confirmed counter and the receipts, fixed the commit path, and deployed only after it held up locally.

### Where AI helped me

- **Sounding board:** I talked through architecture options and stopgaps with it, such as Postgres against Redis, cancel against holds, and how strict to be during an outage.
- **Edge cases:** it helped me list "what if" cases to test, like same-key different-seat retries, cancels racing bookings, and stale cancels after a resale.
- **Diagrams of the flows:** it helped me lay out the transaction steps, the lock order and the request's path through the logs, so I could check them on paper.
- **Drafting:** it drafted code from my design, including the migration 004 constraints, the logging and span code, and the burst script, plus first drafts of the docs and this write-up. I reviewed, corrected or refactored all of it.

### How I checked it

- **Code review:** I read and corrected every change before it went in. Nothing was merged just because it compiled.
- **Tests:** unit tests, plus integration tests against Postgres: one winner under concurrency, and the integrity check catching corrupted data.
- **Burst runs:** `burst.sh` against local Docker and the live URL. I read the full output each time, not just the final `result` line.

## Links and keys

Everything below is a demo value for a throwaway deployment, so it's fine to use and share.

| What | Where |
|---|---|
| Live service | https://api-production-45a5.up.railway.app |
| Source | https://github.com/atuln98/seat-reservation |
| Public dashboard: metrics, logs and spans, no login | https://robustturret3544.grafana.net/public-dashboards/fad3834ab4e9410381c30d957e45401a |
| Admin login (live) | `admin@seat-reservation.demo` / `DemoAdmin-2026!` |
| Metrics bearer token (live) | `27ad4029ca01717562684fac5f2756880f9cb3aab9bc1ef067641a3af854ff0d` |
| Admin login (local Docker) | `admin@local.test` / `admin12345` (`/metrics` is open locally) |

To run the burst against the live service:

```bash
export ADMIN_EMAIL='admin@seat-reservation.demo'
export ADMIN_PASSWORD='DemoAdmin-2026!'
export METRICS_BEARER_TOKEN='27ad4029ca01717562684fac5f2756880f9cb3aab9bc1ef067641a3af854ff0d'
./burst.sh https://api-production-45a5.up.railway.app
```

It takes a bit over a minute and ends with `result=pass` (exit 0) or `result=fail` with a list of what went wrong.

To read the metrics yourself:

```bash
curl -H "Authorization: Bearer 27ad4029ca01717562684fac5f2756880f9cb3aab9bc1ef067641a3af854ff0d" \
  https://api-production-45a5.up.railway.app/metrics
```

To run everything locally, `docker compose up --build` (or `make up`) starts the API on `localhost:8080` next to `postgres:17-alpine`, and the API waits until Postgres is healthy. Then:

```bash
ADMIN_EMAIL='admin@local.test' ADMIN_PASSWORD='admin12345' BURST_CONCURRENCY=1000 ./burst.sh http://localhost:8080
```

Docker on a Mac can't hold 20,000 open connections, so locally the burst still sends all 20,000 requests but keeps 1,000 in flight at a time. Against Railway all 20,000 are in flight at once.

## What it's built with

It's Go (the standard `net/http` plus pgx) in front of PostgreSQL. On Railway it runs as one container built from the repo's Dockerfile, next to a Railway Postgres. The Dockerfile has two stages: `golang:1.25-alpine` builds a static binary, and `alpine:3.22` runs it as a non-root user with only the binary and the SQL migrations. Migrations run at startup under an advisory lock, so several copies can start at once without stepping on each other.

## 1. A show

Only an admin can create a show, so the first question is where the admin comes from. There's no admin sign-up: `POST /auth/register` only ever makes normal users. When the service starts it reads `ADMIN_EMAIL` and `ADMIN_PASSWORD` and creates that admin if the email doesn't exist yet. If it already exists as an admin, nothing happens, and changing the password variable later doesn't reset it. If the email belongs to a normal user, the service refuses to start rather than quietly promote them. Admin routes check the role in the database on every request rather than trusting the token, so demoting someone takes effect immediately.

The admin calls `POST /shows` with a name, a list of seat numbers, a price in paise and optionally a per-user limit (default 4). One row goes into `shows` and one row per seat goes into `seats`, all `available`. Money is a `BIGINT` number of paise everywhere, and price times seats is checked for overflow, so there's no floating point anywhere near it.

`GET /shows/{id}` returns every seat with its state, plus `available`, `held`, `confirmed` and `total_seats`. `available + held + confirmed = total_seats` always holds, because every seat row is in exactly one state.

## 2. A user

A user registers with an email and password and gets a signed token back. From then on the token is the only thing that says who they are. If a request body carries a `user_id` field, the request is rejected with 400, because the body is never allowed to name a user. Register and login are rate limited per IP so they can't be hammered.

## 3. One reservation, step by step

A user sends `POST /shows/{id}/reserve` with `{"seats": ["A12"], "idempotency_key": "..."}`. Here's what happens to that one request.

On the way in, it gets a request ID and a trace ID. If the caller sent an `X-Request-ID` or a `traceparent`, those are kept. Both IDs come back as response headers and appear on every log line and span the request produces. The token is checked, the body is validated (one or more unique seat numbers, no control characters, a key of sensible length, a JSON content type, no unknown fields), and then everything that matters happens inside one Postgres transaction:

1. **Lock the user for this show.** `pg_advisory_xact_lock` on the pair (show, user). Requests from the same user for the same show now run one at a time, and everyone else carries on in parallel.
2. **Look up the idempotency key.** If this user has already used this key, stop here and return the stored reservation (section 4).
3. **Check the limit.** Count the seats this user already holds for this show. If the new seats would take them over the limit, decline with `409 seat_limit_exceeded`. The count is safe because of step 1: nobody else can add seats for this user in between.
4. **Lock the seats.** Lock the seat rows that are still available, in seat-number order:

   ```sql
   SELECT seat_number FROM seats
   WHERE show_id = $1 AND seat_number = ANY($2) AND state = 'available'
   ORDER BY seat_number
   FOR UPDATE
   ```

   If fewer rows come back than seats were asked for, at least one is taken, so the whole request is declined with `409 seats_unavailable` and nothing is booked.
5. **Write the reservation.** One row in `reservations` with the key, the amount and status `confirmed`, and one row per seat in `reservation_seats`.
6. **Flip the seats.** `UPDATE seats SET state = 'confirmed' WHERE ... AND state = 'available'`. If the number of rows changed isn't exactly the number of seats, everything rolls back.
7. **Commit**, then answer `201` with the reservation.

The response looks like the one in the assignment: reservation ID, show, user, seats, `amount_paise` and `status: confirmed`. Afterwards the confirmed counter goes up by one, the request writes one log line, and its spans (auth, each of the steps above, every database call) are exported.

## 4. The same request again

Networks drop, and clients retry. The idempotency key is stored on the reservation row itself, with `UNIQUE (user_id, idempotency_key)`, so keys belong to a user. The key and the reservation are written in the same transaction, so one can never exist without the other.

- **Same key, same seats:** step 2 finds the key and the original reservation comes back with `200` instead of `201`. Nothing new is created and nothing is charged twice.
- **Same key, different seats or show:** `409 idempotency_conflict`, and nothing changes.
- **Many copies at once:** the per-user lock lines them up. The first one books, and the rest find the key and replay it. If two ever got past the lookup together, `ON CONFLICT DO NOTHING` on the insert is a second safety net. In the burst, keys sent 40 times at once always produced exactly one reservation and 39 replays with the same ID.
- **After a cancel:** replaying the key returns the original reservation with `status: cancelled`. It does not book the seats again. Booking again needs a new key.

Keys live as long as the reservation does and never expire.

## 5. Two people, one seat

Say two users want A12 at the same moment. Both transactions reach step 4. The first takes the row lock and the second waits on it. When the first commits, Postgres re-checks `state = 'available'` on the row the second was waiting for, sees `confirmed`, and leaves it out. The second gets zero rows and is declined with a clean 409. Checking and locking happen in one statement, so there's no gap between reading the seat and writing it.

Two more layers back that up, and each would stop a double sale on its own:

- **Guarded update:** the update in step 6 only changes rows that are still `available`.
- **Unique index:** there's a unique partial index on `(show_id, seat_number) WHERE active` in `reservation_seats`, so the database itself refuses a second active owner. That table keeps its own copy of `show_id` and an `active` flag, tied to the parent reservation with a composite foreign key, because an index can't look across a join. If that index ever fires, the service turns it into the same 409.

A request for several seats is all-or-nothing. If any seat is taken, nothing is booked. Deadlocks are avoided by always locking seats in `ORDER BY seat_number`, in both reserve and cancel, and by always taking the advisory lock before any seat lock. Two overlapping requests like `[A1,A2]` and `[A2,A3]` both go for A2 first, so one simply waits.

## 6. Cancelling

I went with explicit cancel (`POST /reservations/{id}/cancel`) rather than time-boxed holds. Reserve is a single step that ends in `confirmed`, so there's nothing to expire, and a timer would only cancel purchases that are already finished. The `held` state exists so the invariant reads like the assignment. Nothing ever puts a seat in it, so it's always 0.

Only the owner can cancel. Anyone else gets 404, so reservation IDs don't leak, and a request without a token gets 401. Cancel takes the same per-user lock, locks the seat rows in order, and releases only seats that are still `confirmed` under this reservation, with a guarded update that must change the expected number of rows. Cancelling something already cancelled returns 200 and touches nothing. A seat that has since been sold to someone else can never be released by the old owner. The burst checks exactly that: it cancels the old reservations again after other people have bought the seats, and the seats must stay with the new owners.

Cancellation works on whole reservations. Someone who booked 2 seats and then 2 more can cancel one of those bookings and book 2 again, but they can't drop one seat out of a 4-seat booking.

## 7. Watching it happen

Every request writes one structured JSON log line to stdout. It's also sent to Grafana Cloud Loki, with `service`, `event`, `requestId`, `traceId` and the reservation details. Each request's spans are exported to Tempo. They're also written to Loki as span log lines (`event="span"`), because Grafana doesn't run Tempo queries for anonymous visitors, so span logs are what lets the public dashboard show the inside of a request without a login. Confirmations, cancellations, 5xx and unexpected failures are always logged. Routine declines are sampled at 10 per second per reason, so a 20,000-request burst doesn't drown the logs. Health checks and metric scrapes aren't logged unless they fail.

To follow one request, take the `X-Request-ID` from its response, or the `sample_request_id` the burst prints. Every burst request is tagged `burst-<run_id>-<index>`. Then search for it. On the public dashboard the log panels already show the latest requests and span logs. With a login, these queries work in Explore:

```logql
{service_name="seat-reservation"} | json | requestId="burst-1791261235-90125-0"
{service_name="seat-reservation"} | json | requestId=~"burst-1791261235-90125-.*" | event="reservation_confirmed"
{service_name="seat-reservation"} | json | event="span" | traceId="<trace id from the request>"
{service_name="seat-reservation"} | json | outcome="reservation_declined" | reason="seats_unavailable"
{service_name="seat-reservation", level="error"}
```

The run ID `1791261235-90125` above is the latest live burst I ran.

`/metrics` (behind the bearer token, scraped by Grafana Cloud every minute) has:
- reservations confirmed (`seat_reservation_reservations_confirmed_total`);
- reservations declined by reason (`seat_taken`, `per_user_limit`, `idempotent_replay`, `idempotency_conflict`);
- available, held, confirmed and total seats per show;
- HTTP request counts and durations by route;
- counters for dropped logs and spans.

The seat gauges are read from Postgres and cached for 5 seconds. `/health/live` only says the process is up. `/health/ready` checks the database and fails closed.

At 2am I'd want to be paged for four things:
- **Integrity:** the integrity check reporting any violation. It should be impossible, and it's logged at ERROR.
- **5xx:** any sustained 5xx, since the target is zero and every expected decline is a 4xx.
- **Readiness:** `/health/ready` failing, or the metrics scrape going down.
- **Saturation:** reserve latency or connection-pool wait climbing, which means the pool is saturating.

Lost logs or spans would just be a ticket. These alerts aren't set up yet. Right now there are only the dashboards.

## 8. When something breaks

I chose consistency over availability. There's no cache, queue or local fallback for seat decisions, so if Postgres can't be reached, reservations fail and `/health/ready` returns 503 while liveness stays up. Nobody can book during that time, but nothing gets sold twice or lost. There's a single Postgres primary and no replica, so there's no split-brain and no failover: losing the database is an outage, not a divergence. `GET /shows/{id}` reads the same database, so it's never stale. Only the Prometheus seat gauges are cached.

If the connection between a client and the service breaks halfway, the client can't know whether it got the seat. That's what the idempotency key is for: retrying with the same key either returns the original reservation or books it exactly once. I found a real bug in this path while running the burst. If a client disconnected at the exact moment its reservation was committing, Postgres could commit while the driver reported the request as cancelled. The reservation existed, and a retry returned it, but the confirmed counter never counted it, so the metrics drifted from the real state. Commits that change state now use a context that the client can't cancel, so they always finish and always get counted. I deployed that fix only after it held up on local Docker: unit tests, the Postgres integration tests, and three full bursts, one with over a thousand deliberately aborted requests.

There's one known gap. A database outage currently comes back as a 500 (or a 408 if the request times out waiting), where a 503 with `Retry-After` would be more honest. I haven't tested a real database outage on the live service.

On scaling, this is one instance with a pool of 16 database connections, not autoscaled. Correctness doesn't depend on the instance count, since every decision is made in Postgres. Only the auth rate limiter, the metric counters and the span collector are per instance.

## 9. Twenty thousand at once

`burst.sh` is the proof. It needs only the admin login (to create a show) and the metrics token (to compare counters before and after). It registers 60 users, creates one fresh show with a limit of 4, and sends 20,000 requests with all of them in flight at once. The requests are shuffled together, so every scenario races every other one:

- **Hot seat:** about 7,500 requests from many users for one seat. Random multi-seat requests also include it.
- **Overlapping groups:** `M1,M2` against `M2,M3`, and `M4,M5,M6` against `M6,M7`, to test the lock ordering.
- **All-or-nothing:** about 900 requests for a free seat together with a sold one.
- **Random seats:** about 7,500 requests for random seats. About 4% are cut off by the client mid-flight and retried later with the same key.
- **Limit with retries:** users firing 40 parallel requests each at a limit of 4, every key sent twice.
- **Idempotency storms:** keys sent 40 times at once, some with the same seat and some alternating between two seats.
- **Cancel races:** owners cancel while their own key is replayed, another user and an anonymous caller try to cancel too, and a dozen people try to grab the freed seat.
- **Hostile requests:** about 15 kinds, from a spoofed `user_id` and a bad token to a 2 MB body and a 300-character key.

Before the burst it walks through idempotency, cancellation and identity one step at a time. Afterwards it replays a sample of keys and cancels the already-cancelled reservations again. It also books up to the limit, cancels part of it, and books again.

The checks don't take the database's word for anything. The script keeps every response it receives and works out from those receipts which reservations should be active, which seats they hold and who owns them. Then it requires that:

- the seats held by active reservations are exactly the seats the API reports as confirmed;
- no seat is held twice and the hot seat is won exactly once;
- nobody is over the limit;
- every idempotency key maps to one reservation;
- every reservation is charged price × seats;
- there are zero 5xx from the service.

While the burst runs it also calls `GET /admin/shows/{id}/integrity` about every 200 ms, and again at the end. That admin endpoint compares the `seats` table against `reservation_seats` and `reservations` in one consistent snapshot. Every confirmed seat must have exactly one active owner, no active owner may sit on an unconfirmed seat or a cancelled reservation, and nobody may be over the limit. `available + held + confirmed = total` is checked as well, but it comes from one table and can't fail by itself, so on its own it proves nothing about double booking. Finally the script checks that the metric counters moved by exactly what it saw.

I also tested the checker itself. I injected a fake double booking, a wrong charge and a fake 500 into a copy of the script, and all were caught. Then I turned 300 real responses into fake edge rejections, and the script retried them and still passed.

### What I learned getting it to a real 20,000

**curl was quietly capping the burst at 300.** The first version passed and printed `concurrency=20000`, but the curl on my machine caps parallel transfers at 300, so only 300 requests were ever in flight. I found this by counting open connections during a run. The script now splits the requests across 80 curl processes of 250 each, holds them all at a start gate and releases them together. You can see it worked in the output: the whole burst takes about as long as the single slowest request, which only happens when nothing is waiting its turn.

**Railway's edge limits each client IP.** At roughly 10,000 requests per second from one IP, Railway's edge starts turning requests away with plain-text 429 and 503 responses before they ever reach the service. In one run that was about 4,400 requests, in another 361, and in another none. The service always answers in JSON, so the script can tell the edge's rejections apart from its own. It retries them with the same idempotency key. Some of them may have reached the service before the edge gave up, so the retry has to return the original rather than book again, which makes it one more idempotency test. A 5xx from the service itself still fails the run. Spreading the load across several machines would avoid the edge limit entirely.

**I caught the commit bug in section 8.** It showed up as the confirmed counter being one lower than the number of reservations the receipts said existed, claude helped me to trace it back by using span logs to commits interrupted by client disconnects.

### The latest live run

This is run `1791261235-90125`, after the fix was deployed:
- **Load:** 20,000 requests in flight at once across 80 curl processes. The burst took 22.7 seconds.
- **Retries:** none were turned away by the edge, and 304 were deliberately aborted by the client and retried with the same key.
- **Hot seat:** 1 winner out of 7,460 tries.
- **5xx:** 0 from the service.
- **Seats:** 96 seats confirmed in the API, exactly the 96 held according to the receipts.
- **Integrity:** 0 violations during and after the burst.
- **Counters:** every metric counter matched exactly what the script saw.

## What I'd do next

- **Database outages:** return 503 with `Retry-After` when the database is down, and test it by actually stopping Postgres.
- **Alerts:** set up the four alerts above.
- **Payments:** if payments were added, move to hold-then-confirm with expiring holds. That means a `held` status with `expires_at`, a confirm endpoint, and expiry guarded the same way the confirm update is.
- **Smaller additions:** per-seat cancel, and a lifetime for idempotency keys.
- **More than one instance:** share the auth rate limit, add up the per-instance counters, and add a Postgres standby with failover.
- **The hot path:** size the pool or put PgBouncer in front, shed load under overload, and limit the per-show metric labels to active shows.
- **Load generation:** run the burst from several machines, so Railway's per-IP edge limit stops being a factor.
- **Public traces:** let anonymous visitors see full traces, either by self-hosting Grafana with anonymous access or with tail sampling so failed requests always keep their whole trace.
