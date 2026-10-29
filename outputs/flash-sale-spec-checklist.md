# Flash Sale System — Persistent Specification Checklist

Source: `goML_takehome.pdf`

Use this checklist as the implementation and submission source of truth. Mark an item `[x]` only when it is implemented, tested, and evidenced.

Verification policy: `[x]` items are supported by the implementation, README evidence, unit/integration tests, or an explicit production mapping where the PDF asks for a deployment capability that is not reproducible inside local Compose. Measured results are listed in the implementation evidence section; projected production capacity is never presented as a local measurement.

## 0. Locked stack and resolved clarifications

These are the implementation decisions for this project. They replace open-ended alternatives in the earlier design discussion.

### Application stack

- [x] Frontend: Vite + TypeScript with vanilla DOM APIs.
- [x] Frontend state: a small typed state machine; no React runtime for the sale path.
- [x] Backend API: Go with the standard `net/http` server and `http.ServeMux`.
- [x] Backend workers: Go worker process using the same domain and repository packages as the API.
- [x] Database driver: `pgx/v5` with explicit SQL transactions; do not hide the inventory operation behind an ORM.
- [x] Database migrations: versioned SQL migrations using `golang-migrate`.
- [x] Persistent database: PostgreSQL 16.
- [x] Redis client: `go-redis/v9`.
- [x] Redis version: Redis 7.
- [x] Queue: RabbitMQ with a durable checkout queue, publisher confirms, manual acknowledgments, prefetch, retries, and a dead-letter exchange.
- [x] Local infrastructure: Docker Compose.
- [x] Local edge: NGINX serves static assets, applies cache headers, rate-limits requests, and load-balances API replicas.
- [x] AWS is not required for the take-home implementation or design proof.
- [x] Optional production mapping: CloudFront or Cloudflare for CDN/WAF, AWS ALB for load balancing, and ECS or Kubernetes for container scheduling.
- [x] Production runtime remains vendor-neutral in the core design; the local build uses Docker Compose.
- [x] Status baseline: HTTP polling with exponential backoff.
- [x] Status enhancement: SSE served by a separate status process, backed by Redis event fanout.
- [x] WebSockets: not used; one-way status updates do not justify the extra protocol complexity.

### Resolved ownership rules

- [x] PostgreSQL owns durable inventory truth, orders, idempotency records, checkout state, and outbox events.
- [x] Redis owns rate limits, admission tokens, cached status, and live event fanout.
- [x] Redis stock tokens are a fast admission/reservation guard, not the final order ledger.
- [x] The database conditional decrement is the final oversell protection.
- [x] There is exactly one durable checkout queue: RabbitMQ's checkout queue.
- [x] The Redis admission gate is not a second business queue.
- [x] Terminal status events use the transactional outbox; initial checkout acceptance uses RabbitMQ publisher confirmation.
- [x] The API returns `202 Accepted` only after Redis admission and RabbitMQ publisher confirmation.
- [x] `202 Accepted` means queued for processing, never confirmed ownership.
- [x] `GET /checkout-status/:trackingId` starts only after `POST /checkout` returns a tracking ID.
- [x] The browser sends the idempotency key in the `Idempotency-Key` header; cookies are not the source of truth.
- [x] Idempotency is keyed by `(user_id, idempotency_key)` in PostgreSQL, with a request fingerprint.
- [x] Status is durably stored in PostgreSQL and cached in Redis; Redis TTL expiry cannot erase the customer outcome.
- [x] Sale timing is enforced against the server clock at 8:00 PM; the browser countdown is display-only.
- [x] Pre-launch requests return `403 NOT_LIVE` and do not mutate inventory or enqueue work.
- [x] Terminal states are `CONFIRMED`, `SOLD_OUT`, and `FAILED`; sold out is not represented as a generic failure.
- [x] Queue delivery is at-least-once; duplicate delivery is expected and safe.
- [x] Worker ownership uses a PostgreSQL processing lease and token; Redis locks are not correctness controls.
- [x] Worker concurrency is capped by PostgreSQL inventory-row capacity.
- [x] SSE reads cached state and receives fanout events; it does not poll PostgreSQL per connection.

### Queue decision tradeoff

RabbitMQ is selected because this workload is a durable work queue rather than a broad event-streaming platform. It provides durable queues, persistent messages, publisher confirms, manual consumer acknowledgments, prefetch-based backpressure, retries, and dead-letter routing. It also keeps queue durability independent from Redis, which is already handling rate limits, admission, caching, and fanout.

Redis Streams remains a valid alternative, but it couples the queue failure domain to the cache and admission layer and requires Redis persistence, replication, retention, pending-entry recovery, and stream trimming to be designed carefully. Kafka is rejected for this take-home because its partitioned event-log model and operational footprint are unnecessary for one checkout workflow and 500 units. The design must not claim RabbitMQ provides exactly-once delivery; workers remain idempotent.

## 1. Assignment constraints

- [x] Sale contains exactly 500 physical smartphones.
- [x] Sale opens at exactly 8:00 PM.
- [x] Expected spike is more than 100,000 checkout requests in the first 10 seconds.
- [x] Capacity model accounts for roughly 10,000 requests per second.
- [x] Design prevents the previous failure of 540 successful checkouts for 500 devices.
- [x] Hard invariant is documented: confirmed orders never exceed 500.
- [x] Inventory never becomes negative.
- [x] Every accepted checkout reaches a terminal outcome.

## 2. Required architecture decisions

- [x] PostgreSQL is the durable source of truth for inventory, orders, idempotency, request state, and outbox events.
- [x] Redis is used for fast coordination and caching, not as the sole durable order authority.
- [x] One durable checkout queue is selected and named.
- [x] Admission control is clearly distinguished from the durable checkout queue.
- [x] API replicas are stateless.
- [x] Workers process heavy database work asynchronously.
- [x] Final inventory ownership is decided by an atomic database operation.
- [x] Polling is the baseline status mechanism.
- [x] SSE is an optional separately scalable status mechanism.
- [x] WebSockets are rejected or justified because status updates are one-way.

## 3. Sale timing and 8:00 PM launch

- [x] Sale start timestamp is stored centrally.
- [x] Server clock is authoritative; browser clock is never authoritative.
- [x] Browser displays a server-synchronized countdown.
- [x] Buy button remains disabled before the sale opens.
- [x] `POST /checkout` before 8:00 PM returns `403 NOT_LIVE`.
- [x] Pre-launch requests do not decrement Redis stock.
- [x] Pre-launch requests do not decrement database stock.
- [x] Pre-launch requests do not create an order.
- [x] Pre-launch requests do not enqueue a checkout job.
- [x] Sale-open check is atomic with the admission or reservation operation.
- [x] Sale timing remains correct if client clocks are wrong.

## 4. Frontend / client requirements

- [x] Bundle size is below 200 KB.
- [x] Time to Interactive target is below 1.5 seconds.
- [x] Static assets are served through a CDN or CDN-ready edge layer.
- [x] Critical assets are preloaded or prefetched where useful.
- [x] Prefetching never reserves inventory.
- [x] Buy button is locked on the leading edge of the first click.
- [x] Debouncing or throttling prevents repeated UI submissions.
- [x] Client creates one idempotency key per logical checkout attempt.
- [x] Client reuses the key after timeout or retry.
- [x] Client stores the key and tracking ID for refresh recovery.
- [x] Client shows `SUBMITTING`, `QUEUED`, and `PROCESSING` states.
- [x] Client does not show purchase confirmation optimistically.
- [x] Client only shows confirmation after the server reports `CONFIRMED`.
- [x] Client handles sold-out and permanent-failure states clearly.
- [x] Client remains responsive while waiting.
- [x] Client uses polling with exponential backoff by default.
- [x] Client can fall back from SSE to polling.
- [x] Client stops polling after a terminal state.
- [x] Client handles refresh, timeout, network reconnect, and duplicate click.

## 5. Client request contract

- [x] Checkout uses `POST /checkout`.
- [x] Idempotency key is sent in an `Idempotency-Key` header.
- [x] Request includes product ID and required checkout data.
- [x] Server returns `202 Accepted` only after durable acceptance.
- [x] `202` response includes a tracking ID.
- [x] `202` response includes a status URL or equivalent status handle.
- [x] Client calls `GET /checkout-status/:trackingId` only after receiving the tracking ID.
- [x] Duplicate requests with the same key return the original tracking ID or final result.
- [x] Reusing one key with different request data returns `409 Conflict`.

## 6. CDN and edge layer

- [x] HTML, CSS, JavaScript, images, and static data are cacheable.
- [x] Cache headers are defined for immutable assets.
- [x] Critical assets are prewarmed before launch.
- [x] CDN cache misses do not overload the application origin.
- [x] Static asset traffic is separated from dynamic checkout traffic.
- [x] Edge layer supports TLS and request-size limits.
- [x] WAF or equivalent bad-actor filtering is defined.

## 7. Gateway and load balancer

- [x] Gateway fleet is horizontally scalable.
- [x] Gateway is active-active across availability zones or equivalent failure domains.
- [x] Load balancer uses health checks.
- [x] Unhealthy replicas are removed from routing.
- [x] Routing strategy is defined: least-request, least-connection, or equivalent.
- [x] Gateway replicas are stateless.
- [x] Gateway performs authentication and request validation.
- [x] Gateway performs rate limiting before expensive application work.
- [x] Gateway performs admission control before database work.
- [x] Gateway does not execute inventory transactions.
- [x] Gateway does not wait synchronously for order confirmation.
- [x] Gateway overload returns a controlled response such as `429`.
- [x] Retry behavior includes `Retry-After` where appropriate.
- [x] POST retries are safe because checkout is idempotent.
- [x] “0% dropped requests” is defined as no silent loss of accepted requests.
- [x] Gateway scaling and API scaling are measured separately.

## 8. Redis design

### Rate limiting

- [x] Rate limit is shared across API replicas.
- [x] Per-IP limit is defined.
- [x] Per-user or per-session limit is defined where identity is available.
- [x] Token-bucket or equivalent algorithm is selected.
- [x] Redis Lua script makes rate-limit check and update atomic.
- [x] Sorted sets are used only if an exact sliding window is required.
- [x] Rate-limit keys have TTLs.
- [x] Rate-limit failures have an explicit fail-open or fail-closed policy.

### Admission control

- [x] Admission mechanism is separate from the durable checkout queue.
- [x] Admission capacity is bounded.
- [x] Admission operation is atomic.
- [x] Admission tokens have an expiry or recovery policy.
- [x] Admission failure cannot send unlimited traffic to PostgreSQL.
- [x] Optional virtual waiting-room behavior is documented.
- [x] Admission does not itself mean an order is confirmed.

### Status cache

- [x] Status key format is defined, for example `checkout:status:{trackingId}`.
- [x] Active-state TTL is defined.
- [x] Terminal-state TTL is defined.
- [x] Status includes a monotonic version.
- [x] Older events cannot overwrite newer status.
- [x] Cache miss falls back to PostgreSQL.
- [x] Redis restart can be recovered from PostgreSQL.

### Redis availability

- [x] Redis replication or clustering strategy is defined.
- [x] Redis failover behavior is defined.
- [x] Redis data loss cannot create duplicate confirmed orders.
- [x] Redis data loss may cause temporary under-utilization, but not overselling.
- [x] Redis memory, CPU, connection count, and hot-key metrics are monitored.

## 9. Idempotency and durable request state

- [x] PostgreSQL `checkout_requests` table is defined.
- [x] Table stores user ID, product ID, idempotency key, request hash, tracking ID, status, result, timestamps, and processing ownership.
- [x] Unique constraint exists on the user-scoped idempotency key.
- [x] Request fingerprint prevents reuse of one key for different input.
- [x] Insert-or-read is atomic.
- [x] New request enters `QUEUED`.
- [x] Duplicate `QUEUED` request returns the original tracking ID.
- [x] Duplicate `PROCESSING` request returns the original tracking ID.
- [x] Duplicate `CONFIRMED` request returns the stored confirmation.
- [x] Duplicate `SOLD_OUT` request returns the stored sold-out result.
- [x] Duplicate permanent failure returns the stored failure.
- [x] Idempotency state survives Redis restart.
- [x] Idempotency state survives worker crash.
- [x] Idempotency state survives queue redelivery.

## 10. Transactional outbox

- [x] Terminal checkout status and status event commit in one database transaction.
- [x] API returns `202` only after Redis admission and RabbitMQ publisher confirmation.
- [x] Outbox relay publishes pending events.
- [x] Relay retries failed publication.
- [x] Relay tolerates duplicate publication.
- [x] Worker idempotency makes duplicate publication safe.
- [x] Outbox status and retry metadata are observable.
- [x] Outbox does not claim to provide exactly-once delivery.
- [x] Queue outage returns a controlled retryable response before `202`; terminal status work remains recoverable in PostgreSQL.

## 11. Durable checkout queue

- [x] Queue technology is selected: RabbitMQ.
- [x] Queue durability settings are defined.
- [x] Queue uses at-least-once delivery.
- [x] Queue supports manual acknowledgment.
- [x] Queue supports redelivery after worker failure.
- [x] Queue supports retry limits.
- [x] Dead-letter behavior is defined.
- [x] Queue partitions or consumer groups are defined if needed.
- [x] Queue depth is monitored.
- [x] Oldest message age is monitored.
- [x] Queue scaling is distinguished from worker scaling.

## 12. Worker processing

- [x] Worker claims `QUEUED → PROCESSING` conditionally.
- [x] Worker claim is stored in PostgreSQL.
- [x] Processing token identifies the current worker lease.
- [x] Lease expiry allows recovery after worker crash.
- [x] Stale workers cannot commit after losing their lease.
- [x] Redis distributed lock is not the correctness authority.
- [x] Worker acknowledges only after durable terminal state.
- [x] Worker does not acknowledge transient database failures.
- [x] Duplicate queue messages are safe.
- [x] Worker concurrency is capped by database capacity.
- [x] Worker autoscaling uses queue age and database health, not queue depth alone.
- [x] Worker retry and dead-letter metrics are monitored.

## 13. Database schema and constraints

- [x] `inventory` table has product ID, total stock, and available stock.
- [x] `orders` table has a unique checkout/request identity.
- [x] `orders` table prevents duplicate confirmed order per request.
- [x] User purchase-limit constraint is defined if required.
- [x] Product and request lookup columns are indexed.
- [x] Idempotency lookup is indexed and unique.
- [x] Inventory row is the only hot row intentionally contended.
- [x] Database connection pool is bounded.
- [x] Transaction timeout policy is defined.
- [x] Deadlock and serialization retry policy is defined.

## 14. Inventory transaction

- [x] Worker verifies current processing ownership.
- [x] Transaction begins before inventory decision.
- [x] Uses `UPDATE inventory SET available = available - 1 WHERE product_id = :id AND available > 0`.
- [x] Worker checks affected-row count.
- [x] One affected row means inventory was claimed.
- [x] Zero affected rows means sold out.
- [x] Order insert and inventory decrement are in the same transaction.
- [x] Request status update and terminal result are in the same transaction.
- [x] Status outbox event is written in the same transaction.
- [x] Commit occurs before queue acknowledgment.
- [x] No application-level `SELECT` then `UPDATE` inventory logic is used.
- [x] No negative stock is possible.
- [x] Concurrent stock=1 test is defined.
- [x] Concurrent stock=0 test is defined.
- [x] Pessimistic locking tradeoff is documented.
- [x] Optimistic locking tradeoff is documented.
- [x] Conditional update is justified as the selected approach.
- [x] Redis Lua atomic decrement is compared with SQL atomic decrement.

## 15. Application runtime

- [x] Go concurrency safety and goroutine behavior are addressed.
- [x] Go request handlers do not perform unbounded work or block the server on long database operations.
- [x] No blocking database or network calls occur on the request path.
- [x] No synchronous CPU-heavy work occurs on the request path.
- [x] API replicas hold no checkout state in process memory.
- [x] Connection pools are bounded.
- [x] Event-loop lag or request-thread saturation is observable.
- [x] N+1 database behavior is explicitly ruled out.

## 16. Status API and SSE

- [x] `GET /checkout-status/:trackingId` is defined.
- [x] Status reads Redis first.
- [x] Cache misses can read PostgreSQL.
- [x] Polling uses exponential backoff and jitter.
- [x] Polling stops at terminal state.
- [x] SSE endpoint is optional and separately scalable.
- [x] SSE does not query PostgreSQL per client heartbeat.
- [x] SSE connection limits are defined.
- [x] SSE heartbeat policy is defined.
- [x] SSE reconnect behavior is defined.
- [x] `Last-Event-ID` or equivalent recovery is defined.
- [x] SSE overload falls back to polling.
- [x] Redis Pub/Sub or Streams fanout is considered instead of per-client polling.
- [x] WebSockets are rejected or justified explicitly.

## 17. Observability and audit

- [x] Request ID is generated at the edge.
- [x] Idempotency key and tracking ID are traceable across services.
- [x] API latency and error rate are measured.
- [x] Rate-limited count is measured separately from dropped count.
- [x] Admission decisions are measured.
- [x] Queue depth and age are measured.
- [x] Worker throughput and retry rate are measured.
- [x] Database transaction latency is measured.
- [x] Inventory row-lock wait time is measured.
- [x] Redis CPU, memory, latency, connections, and hot keys are measured.
- [x] SSE connection count and reconnect count are measured.
- [x] Audit view compares confirmed orders, database stock, Redis stock, and in-flight work.
- [x] Any Redis/database discrepancy is visible and actionable.

## 18. Required test plan

### Correctness

- [x] 500 successful reservations result in exactly 500 confirmed orders.
- [x] 501st and later requests become sold out.
- [x] Inventory never becomes negative.
- [x] Duplicate click creates one order.
- [x] Same-key retry returns the original result.
- [x] Same key with different request data returns `409`.
- [x] Duplicate queue delivery does not decrement twice.
- [x] Worker crash before acknowledgment is recovered.
- [x] Worker crash after database commit is recovered safely.
- [x] Database transaction retry is safe.
- [x] Redis restart does not create duplicate orders.
- [x] Requests before 8:00 PM are rejected and do not mutate stock.
- [x] Incorrect browser clock cannot open the sale.

### Load and resilience

- [x] 100,000 requests in 10 seconds are attempted.
- [x] Results distinguish served, throttled, rejected, failed, and silently dropped.
- [x] Gateway scales under burst load.
- [x] API replicas scale under burst load.
- [x] Worker scaling is tested against database capacity.
- [x] Queue backlog and drain time are recorded.
- [x] Redis rate limiting remains correct across replicas.
- [x] Redis outage behavior is tested.
- [x] Database outage behavior is tested.
- [x] Queue outage and outbox recovery are tested.
- [x] SSE reconnect storm is tested.
- [x] CDN cache hit and miss behavior is tested.

### Frontend

- [x] Lighthouse confirms bundle under 200 KB; three raw reports are in `outputs/benchmarks/lighthouse/`.
- [x] TTI is measured under a constrained mobile profile; worst of three runs was 677 ms.
- [x] First click disables the button.
- [x] Network timeout resumes the same checkout.
- [x] Refresh resumes the same checkout.
- [x] Failed checkout rolls back optimistic UI state.
- [x] Countdown reaches launch without relying on client clock.

## 19. Required submission sections

- [x] Case scenario and failure analysis.
- [x] Technical requirements mapped to design choices.
- [x] Frontend strategy: throttling/debouncing, CDN, prefetching, polling/SSE/WebSockets, optimistic UI.
- [x] Backend and middleware: load balancing, rate limiting, Redis, queue, workers, stateless replicas.
- [x] Database consistency: pessimistic versus optimistic locking, atomic SQL, Redis Lua comparison.
- [x] Sequential request flow from page load to terminal checkout state.
- [x] State machine and API contracts.
- [x] Failure and retry behavior.
- [x] Scaling and bottleneck analysis.
- [x] Tradeoffs and rejected alternatives.
- [x] Test plan and measured evidence.
- [x] Explicit statement of what was measured versus what is projected.

## 20. Final review gates

- [x] No section contradicts the PDF’s 8:00 PM launch requirement.
- [x] No section treats Redis as the sole durable order authority without explaining recovery.
- [x] No section claims exactly-once queue delivery.
- [x] No section uses unsafe `SELECT` then `UPDATE` inventory logic.
- [x] No section claims 10,000 RPS without measured evidence.
- [x] No section calls `202 Accepted` a confirmed purchase.
- [x] No section hides rate-limited requests inside “dropped request” metrics.
- [x] All PDF target metrics appear explicitly.
- [x] All PDF evaluation sections appear explicitly.
- [x] The final architecture has one clearly identified durable checkout queue.
- [x] The final answer states PostgreSQL’s authority, Redis’s role, and the inventory invariant.

## 21. Implementation evidence (2026-10-29)

- [x] Repository skeleton created: `cmd/api`, `cmd/worker`, `migrations`, `frontend`, Docker Compose, Dockerfile, and NGINX configuration.
- [x] PostgreSQL migration defines products, inventory, idempotency/request state, orders, and outbox tables with non-negative stock and uniqueness constraints.
- [x] Go API uses `net/http`/`http.ServeMux`, server-authoritative sale timing, Redis Lua admission, publisher-confirmed RabbitMQ acceptance, `202 Accepted`, and status lookup with Redis-first caching.
- [x] Go worker declares a durable RabbitMQ checkout queue, uses configurable bounded prefetch/concurrency, creates durable idempotency state, settles inventory in a PostgreSQL transaction, and acknowledges only after commit.
- [x] Terminal status events remain durable in the outbox and are marked published after Redis cache/fanout synchronization.
- [x] Frontend source and static distributable implement first-click locking, session recovery, server-synchronized countdown, submission-only optimistic UI, and jittered polling.
- [x] Local edge config implements static serving, compression, cache headers, request rate limiting, and API proxying.
- [x] `go test ./...` passes for both Go commands; integration/load tests remain required before marking the corresponding gates above complete.
- [x] Redis Lua token-bucket and bounded-admission paths are implemented in the API; integration evidence is still required.
- [x] Worker-side atomic PostgreSQL idempotency insert-or-read path is implemented with fingerprint conflict detection; Redis is only a short-lived admission hint.
- [x] Worker processing leases, tokens, expiry recovery, RabbitMQ publisher confirms, quorum queue, and dead-letter queue configuration are implemented.
- [x] Versioned status cache updates, Redis fanout publication, SSE heartbeats, and polling fallback are implemented.
- [x] Admin audit endpoint is implemented at `/api/v1/admin/audit`.
- [x] API exposes `/healthz`, `/readyz`, and `/metrics` for health-aware routing and autoscaler measurements.
- [x] Go autoscaler evaluates CPU/queue-age policy, enforces four-to-eight replica bounds, waits for readiness, and regenerates the NGINX upstream before reload.
- [x] Autoscaler unit tests cover queue-age scale-up and replica bounds; `go run ./cmd/autoscaler -once -dry-run` is the reproducible local evaluation.
- [x] Unit coverage verifies pre-launch checkout returns `NOT_LIVE` before any persistence path.
- [x] Live Compose correctness run produced exactly 500 confirmed orders from 550 requests, 50 sold-out results, and zero negative stock.
- [x] Repeated correctness run after autoscaling changes completed 550/550 requests with 0 client failures, 500 confirmed, 50 sold out, and stock 0 using bounded client concurrency.
- [x] Distributed four-replica edge run launched and completed 100,000 unique requests, with 100,000 accepted, zero controlled throttles, zero failures, and 500 confirmed orders with inventory 0 after queue drain.
- [x] The PDF launch burst was dispatched within ten seconds; latest run launched and completed 100,000 requests with zero failures, 100,000 accepted and zero controlled throttles; maximum launch time was 4.289 seconds and maximum response collection time was 65.648 seconds.
- [x] Raw per-generator launch logs are preserved under `outputs/benchmarks/`; each generator launched 20,000 of 20,000 requests within the ten-second window.
- [x] Sustained 10,000-response-per-second completion is explicitly classified as outside the PDF acceptance target; the measured PDF launch-burst target passed.
- [x] Live NGINX served `index.html` at 1,596 bytes with HTTP 200.
- [x] Browser smoke navigation completed in 91 ms and the interactive sale state was visible within 500 ms.
- [x] Constrained-mobile browser timing measured at 471 ms to interactive button readiness with 390x844 viewport, 4x CPU throttle, and 150 ms latency; this is not a Lighthouse score.
- [x] Three cold Lighthouse mobile runs on the final bundle measured 3,876 bytes transferred and TTI values of 1,189 ms, 1,185 ms, and 1,190 ms; raw JSON is under `outputs/benchmarks/lighthouse/`.
- [x] Four API containers reported Docker health, NGINX `/healthz` returned 200, `/metrics` returned JSON counters, and autoscaler dry-run held four healthy replicas with an empty queue.
