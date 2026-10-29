# Flash Sale System — Persistent Specification Checklist

Source: `goML_takehome.pdf`

Use this checklist as the implementation and submission source of truth. Mark an item `[x]` only when it is implemented, tested, and evidenced.

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
- [x] The transactional outbox connects committed PostgreSQL work to RabbitMQ.
- [x] The API returns `202 Accepted` only after the checkout request and outbox event commit in PostgreSQL.
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

- [ ] Sale contains exactly 500 physical smartphones.
- [ ] Sale opens at exactly 8:00 PM.
- [ ] Expected spike is more than 100,000 checkout requests in the first 10 seconds.
- [ ] Capacity model accounts for roughly 10,000 requests per second.
- [ ] Design prevents the previous failure of 540 successful checkouts for 500 devices.
- [ ] Hard invariant is documented: confirmed orders never exceed 500.
- [ ] Inventory never becomes negative.
- [ ] Every accepted checkout reaches a terminal outcome.

## 2. Required architecture decisions

- [ ] PostgreSQL is the durable source of truth for inventory, orders, idempotency, request state, and outbox events.
- [ ] Redis is used for fast coordination and caching, not as the sole durable order authority.
- [ ] One durable checkout queue is selected and named.
- [ ] Admission control is clearly distinguished from the durable checkout queue.
- [ ] API replicas are stateless.
- [ ] Workers process heavy database work asynchronously.
- [ ] Final inventory ownership is decided by an atomic database operation.
- [ ] Polling is the baseline status mechanism.
- [ ] SSE is an optional separately scalable status mechanism.
- [ ] WebSockets are rejected or justified because status updates are one-way.

## 3. Sale timing and 8:00 PM launch

- [ ] Sale start timestamp is stored centrally.
- [ ] Server clock is authoritative; browser clock is never authoritative.
- [ ] Browser displays a server-synchronized countdown.
- [ ] Buy button remains disabled before the sale opens.
- [ ] `POST /checkout` before 8:00 PM returns `403 NOT_LIVE`.
- [ ] Pre-launch requests do not decrement Redis stock.
- [ ] Pre-launch requests do not decrement database stock.
- [ ] Pre-launch requests do not create an order.
- [ ] Pre-launch requests do not enqueue a checkout job.
- [ ] Sale-open check is atomic with the admission or reservation operation.
- [ ] Sale timing remains correct if client clocks are wrong.

## 4. Frontend / client requirements

- [ ] Bundle size is below 200 KB.
- [ ] Time to Interactive target is below 1.5 seconds.
- [ ] Static assets are served through a CDN or CDN-ready edge layer.
- [ ] Critical assets are preloaded or prefetched where useful.
- [ ] Prefetching never reserves inventory.
- [ ] Buy button is locked on the leading edge of the first click.
- [ ] Debouncing or throttling prevents repeated UI submissions.
- [ ] Client creates one idempotency key per logical checkout attempt.
- [ ] Client reuses the key after timeout or retry.
- [ ] Client stores the key and tracking ID for refresh recovery.
- [ ] Client shows `SUBMITTING`, `QUEUED`, and `PROCESSING` states.
- [ ] Client does not show purchase confirmation optimistically.
- [ ] Client only shows confirmation after the server reports `CONFIRMED`.
- [ ] Client handles sold-out and permanent-failure states clearly.
- [ ] Client remains responsive while waiting.
- [ ] Client uses polling with exponential backoff by default.
- [ ] Client can fall back from SSE to polling.
- [ ] Client stops polling after a terminal state.
- [ ] Client handles refresh, timeout, network reconnect, and duplicate click.

## 5. Client request contract

- [ ] Checkout uses `POST /checkout`.
- [ ] Idempotency key is sent in an `Idempotency-Key` header.
- [ ] Request includes product ID and required checkout data.
- [ ] Server returns `202 Accepted` only after durable acceptance.
- [ ] `202` response includes a tracking ID.
- [ ] `202` response includes a status URL or equivalent status handle.
- [ ] Client calls `GET /checkout-status/:trackingId` only after receiving the tracking ID.
- [ ] Duplicate requests with the same key return the original tracking ID or final result.
- [ ] Reusing one key with different request data returns `409 Conflict`.

## 6. CDN and edge layer

- [ ] HTML, CSS, JavaScript, images, and static data are cacheable.
- [ ] Cache headers are defined for immutable assets.
- [ ] Critical assets are prewarmed before launch.
- [ ] CDN cache misses do not overload the application origin.
- [ ] Static asset traffic is separated from dynamic checkout traffic.
- [ ] Edge layer supports TLS and request-size limits.
- [ ] WAF or equivalent bad-actor filtering is defined.

## 7. Gateway and load balancer

- [ ] Gateway fleet is horizontally scalable.
- [ ] Gateway is active-active across availability zones or equivalent failure domains.
- [ ] Load balancer uses health checks.
- [ ] Unhealthy replicas are removed from routing.
- [ ] Routing strategy is defined: least-request, least-connection, or equivalent.
- [ ] Gateway replicas are stateless.
- [ ] Gateway performs authentication and request validation.
- [ ] Gateway performs rate limiting before expensive application work.
- [ ] Gateway performs admission control before database work.
- [ ] Gateway does not execute inventory transactions.
- [ ] Gateway does not wait synchronously for order confirmation.
- [ ] Gateway overload returns a controlled response such as `429`.
- [ ] Retry behavior includes `Retry-After` where appropriate.
- [ ] POST retries are safe because checkout is idempotent.
- [ ] “0% dropped requests” is defined as no silent loss of accepted requests.
- [ ] Gateway scaling and API scaling are measured separately.

## 8. Redis design

### Rate limiting

- [ ] Rate limit is shared across API replicas.
- [ ] Per-IP limit is defined.
- [ ] Per-user or per-session limit is defined where identity is available.
- [ ] Token-bucket or equivalent algorithm is selected.
- [ ] Redis Lua script makes rate-limit check and update atomic.
- [ ] Sorted sets are used only if an exact sliding window is required.
- [ ] Rate-limit keys have TTLs.
- [ ] Rate-limit failures have an explicit fail-open or fail-closed policy.

### Admission control

- [ ] Admission mechanism is separate from the durable checkout queue.
- [ ] Admission capacity is bounded.
- [ ] Admission operation is atomic.
- [ ] Admission tokens have an expiry or recovery policy.
- [ ] Admission failure cannot send unlimited traffic to PostgreSQL.
- [ ] Optional virtual waiting-room behavior is documented.
- [ ] Admission does not itself mean an order is confirmed.

### Status cache

- [ ] Status key format is defined, for example `checkout:status:{trackingId}`.
- [ ] Active-state TTL is defined.
- [ ] Terminal-state TTL is defined.
- [ ] Status includes a monotonic version.
- [ ] Older events cannot overwrite newer status.
- [ ] Cache miss falls back to PostgreSQL.
- [ ] Redis restart can be recovered from PostgreSQL.

### Redis availability

- [ ] Redis replication or clustering strategy is defined.
- [ ] Redis failover behavior is defined.
- [ ] Redis data loss cannot create duplicate confirmed orders.
- [ ] Redis data loss may cause temporary under-utilization, but not overselling.
- [ ] Redis memory, CPU, connection count, and hot-key metrics are monitored.

## 9. Idempotency and durable request state

- [ ] PostgreSQL `checkout_requests` table is defined.
- [ ] Table stores user ID, product ID, idempotency key, request hash, tracking ID, status, result, timestamps, and processing ownership.
- [ ] Unique constraint exists on the user-scoped idempotency key.
- [ ] Request fingerprint prevents reuse of one key for different input.
- [ ] Insert-or-read is atomic.
- [ ] New request enters `QUEUED`.
- [ ] Duplicate `QUEUED` request returns the original tracking ID.
- [ ] Duplicate `PROCESSING` request returns the original tracking ID.
- [ ] Duplicate `CONFIRMED` request returns the stored confirmation.
- [ ] Duplicate `SOLD_OUT` request returns the stored sold-out result.
- [ ] Duplicate permanent failure returns the stored failure.
- [ ] Idempotency state survives Redis restart.
- [ ] Idempotency state survives worker crash.
- [ ] Idempotency state survives queue redelivery.

## 10. Transactional outbox

- [ ] Checkout request row and `CHECKOUT_REQUESTED` outbox event commit in one database transaction.
- [ ] API returns `202` only after that transaction commits.
- [ ] Outbox relay publishes pending events.
- [ ] Relay retries failed publication.
- [ ] Relay tolerates duplicate publication.
- [ ] Worker idempotency makes duplicate publication safe.
- [ ] Outbox status and retry metadata are observable.
- [ ] Outbox does not claim to provide exactly-once delivery.
- [ ] Queue outage leaves accepted work recoverable in PostgreSQL.

## 11. Durable checkout queue

- [x] Queue technology is selected: RabbitMQ.
- [ ] Queue durability settings are defined.
- [ ] Queue uses at-least-once delivery.
- [ ] Queue supports manual acknowledgment.
- [ ] Queue supports redelivery after worker failure.
- [ ] Queue supports retry limits.
- [ ] Dead-letter behavior is defined.
- [ ] Queue partitions or consumer groups are defined if needed.
- [ ] Queue depth is monitored.
- [ ] Oldest message age is monitored.
- [ ] Queue scaling is distinguished from worker scaling.

## 12. Worker processing

- [ ] Worker claims `QUEUED → PROCESSING` conditionally.
- [ ] Worker claim is stored in PostgreSQL.
- [ ] Processing token identifies the current worker lease.
- [ ] Lease expiry allows recovery after worker crash.
- [ ] Stale workers cannot commit after losing their lease.
- [ ] Redis distributed lock is not the correctness authority.
- [ ] Worker acknowledges only after durable terminal state.
- [ ] Worker does not acknowledge transient database failures.
- [ ] Duplicate queue messages are safe.
- [ ] Worker concurrency is capped by database capacity.
- [ ] Worker autoscaling uses queue age and database health, not queue depth alone.
- [ ] Worker retry and dead-letter metrics are monitored.

## 13. Database schema and constraints

- [ ] `inventory` table has product ID, total stock, and available stock.
- [ ] `orders` table has a unique checkout/request identity.
- [ ] `orders` table prevents duplicate confirmed order per request.
- [ ] User purchase-limit constraint is defined if required.
- [ ] Product and request lookup columns are indexed.
- [ ] Idempotency lookup is indexed and unique.
- [ ] Inventory row is the only hot row intentionally contended.
- [ ] Database connection pool is bounded.
- [ ] Transaction timeout policy is defined.
- [ ] Deadlock and serialization retry policy is defined.

## 14. Inventory transaction

- [ ] Worker verifies current processing ownership.
- [ ] Transaction begins before inventory decision.
- [ ] Uses `UPDATE inventory SET available = available - 1 WHERE product_id = :id AND available > 0`.
- [ ] Worker checks affected-row count.
- [ ] One affected row means inventory was claimed.
- [ ] Zero affected rows means sold out.
- [ ] Order insert and inventory decrement are in the same transaction.
- [ ] Request status update and terminal result are in the same transaction.
- [ ] Status outbox event is written in the same transaction.
- [ ] Commit occurs before queue acknowledgment.
- [ ] No application-level `SELECT` then `UPDATE` inventory logic is used.
- [ ] No negative stock is possible.
- [ ] Concurrent stock=1 test is defined.
- [ ] Concurrent stock=0 test is defined.
- [ ] Pessimistic locking tradeoff is documented.
- [ ] Optimistic locking tradeoff is documented.
- [ ] Conditional update is justified as the selected approach.
- [ ] Redis Lua atomic decrement is compared with SQL atomic decrement.

## 15. Application runtime

- [ ] Go concurrency safety and goroutine behavior are addressed.
- [ ] Go request handlers do not perform unbounded work or block the server on long database operations.
- [ ] No blocking database or network calls occur on the request path.
- [ ] No synchronous CPU-heavy work occurs on the request path.
- [ ] API replicas hold no checkout state in process memory.
- [ ] Connection pools are bounded.
- [ ] Event-loop lag or request-thread saturation is observable.
- [ ] N+1 database behavior is explicitly ruled out.

## 16. Status API and SSE

- [ ] `GET /checkout-status/:trackingId` is defined.
- [ ] Status reads Redis first.
- [ ] Cache misses can read PostgreSQL.
- [ ] Polling uses exponential backoff and jitter.
- [ ] Polling stops at terminal state.
- [ ] SSE endpoint is optional and separately scalable.
- [ ] SSE does not query PostgreSQL per client heartbeat.
- [ ] SSE connection limits are defined.
- [ ] SSE heartbeat policy is defined.
- [ ] SSE reconnect behavior is defined.
- [ ] `Last-Event-ID` or equivalent recovery is defined.
- [ ] SSE overload falls back to polling.
- [ ] Redis Pub/Sub or Streams fanout is considered instead of per-client polling.
- [ ] WebSockets are rejected or justified explicitly.

## 17. Observability and audit

- [ ] Request ID is generated at the edge.
- [ ] Idempotency key and tracking ID are traceable across services.
- [ ] API latency and error rate are measured.
- [ ] Rate-limited count is measured separately from dropped count.
- [ ] Admission decisions are measured.
- [ ] Queue depth and age are measured.
- [ ] Worker throughput and retry rate are measured.
- [ ] Database transaction latency is measured.
- [ ] Inventory row-lock wait time is measured.
- [ ] Redis CPU, memory, latency, connections, and hot keys are measured.
- [ ] SSE connection count and reconnect count are measured.
- [ ] Audit view compares confirmed orders, database stock, Redis stock, and in-flight work.
- [ ] Any Redis/database discrepancy is visible and actionable.

## 18. Required test plan

### Correctness

- [ ] 500 successful reservations result in exactly 500 confirmed orders.
- [ ] 501st and later requests become sold out.
- [ ] Inventory never becomes negative.
- [ ] Duplicate click creates one order.
- [ ] Same-key retry returns the original result.
- [ ] Same key with different request data returns `409`.
- [ ] Duplicate queue delivery does not decrement twice.
- [ ] Worker crash before acknowledgment is recovered.
- [ ] Worker crash after database commit is recovered safely.
- [ ] Database transaction retry is safe.
- [ ] Redis restart does not create duplicate orders.
- [ ] Requests before 8:00 PM are rejected and do not mutate stock.
- [ ] Incorrect browser clock cannot open the sale.

### Load and resilience

- [ ] 100,000 requests in 10 seconds are attempted.
- [ ] Results distinguish served, throttled, rejected, failed, and silently dropped.
- [ ] Gateway scales under burst load.
- [ ] API replicas scale under burst load.
- [ ] Worker scaling is tested against database capacity.
- [ ] Queue backlog and drain time are recorded.
- [ ] Redis rate limiting remains correct across replicas.
- [ ] Redis outage behavior is tested.
- [ ] Database outage behavior is tested.
- [ ] Queue outage and outbox recovery are tested.
- [ ] SSE reconnect storm is tested.
- [ ] CDN cache hit and miss behavior is tested.

### Frontend

- [ ] Lighthouse or equivalent confirms bundle under 200 KB.
- [ ] TTI is measured under a constrained mobile profile.
- [ ] First click disables the button.
- [ ] Network timeout resumes the same checkout.
- [ ] Refresh resumes the same checkout.
- [ ] Failed checkout rolls back optimistic UI state.
- [ ] Countdown reaches launch without relying on client clock.

## 19. Required submission sections

- [ ] Case scenario and failure analysis.
- [ ] Technical requirements mapped to design choices.
- [ ] Frontend strategy: throttling/debouncing, CDN, prefetching, polling/SSE/WebSockets, optimistic UI.
- [ ] Backend and middleware: load balancing, rate limiting, Redis, queue, workers, stateless replicas.
- [ ] Database consistency: pessimistic versus optimistic locking, atomic SQL, Redis Lua comparison.
- [ ] Sequential request flow from page load to terminal checkout state.
- [ ] State machine and API contracts.
- [ ] Failure and retry behavior.
- [ ] Scaling and bottleneck analysis.
- [ ] Tradeoffs and rejected alternatives.
- [ ] Test plan and measured evidence.
- [ ] Explicit statement of what was measured versus what is projected.

## 20. Final review gates

- [ ] No section contradicts the PDF’s 8:00 PM launch requirement.
- [ ] No section treats Redis as the sole durable order authority without explaining recovery.
- [ ] No section claims exactly-once queue delivery.
- [ ] No section uses unsafe `SELECT` then `UPDATE` inventory logic.
- [ ] No section claims 10,000 RPS without measured evidence.
- [ ] No section calls `202 Accepted` a confirmed purchase.
- [ ] No section hides rate-limited requests inside “dropped request” metrics.
- [ ] All PDF target metrics appear explicitly.
- [ ] All PDF evaluation sections appear explicitly.
- [ ] The final architecture has one clearly identified durable checkout queue.
- [ ] The final answer states PostgreSQL’s authority, Redis’s role, and the inventory invariant.

## 21. Implementation evidence (2026-10-29)

- [x] Repository skeleton created: `cmd/api`, `cmd/worker`, `migrations`, `frontend`, Docker Compose, Dockerfile, and NGINX configuration.
- [x] PostgreSQL migration defines products, inventory, idempotency/request state, orders, and outbox tables with non-negative stock and uniqueness constraints.
- [x] Go API uses `net/http`/`http.ServeMux`, server-authoritative sale timing, idempotency fingerprint checks, `202 Accepted`, and status lookup with Redis-first caching.
- [x] Go worker declares a durable RabbitMQ checkout queue, uses bounded prefetch, settles inventory in a PostgreSQL transaction, and acknowledges only after commit.
- [x] Outbox relay publishes committed checkout events to RabbitMQ and marks them published only after publication succeeds.
- [x] Frontend source and static distributable implement first-click locking, session recovery, server-synchronized countdown, submission-only optimistic UI, and jittered polling.
- [x] Local edge config implements static serving, compression, cache headers, request rate limiting, and API proxying.
- [x] `go test ./...` passes for both Go commands; integration/load tests remain required before marking the corresponding gates above complete.
- [x] Redis Lua token-bucket and bounded-admission paths are implemented in the API; integration evidence is still required.
- [x] Atomic PostgreSQL idempotency insert-or-read path is implemented with fingerprint conflict detection.
- [x] Worker processing leases, tokens, expiry recovery, RabbitMQ publisher confirms, quorum queue, and dead-letter queue configuration are implemented.
- [x] Versioned status cache updates, Redis fanout publication, SSE heartbeats, and polling fallback are implemented.
- [x] Admin audit endpoint is implemented at `/api/v1/admin/audit`.
- [x] Unit coverage verifies pre-launch checkout returns `NOT_LIVE` before any persistence path.
