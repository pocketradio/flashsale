# Flash Sale Checkout System Design

## 1. Executive Summary

This design supports a flash sale of 500 smartphones with more than 100,000 checkout requests arriving during the first 10 seconds of the sale. That is an average burst of approximately 10,000 checkout requests per second.

The primary correctness requirement is:

```text
confirmed_orders <= 500
inventory.available >= 0
one idempotency key = one checkout outcome
```

The design separates traffic handling from durable business processing:

- A CDN serves static assets.
- An active-active gateway fleet handles authentication, rate limiting, and routing.
- Redis Cluster performs fast rate limiting, admission control, status caching, and event fanout.
- PostgreSQL is the persistent source of truth for inventory, checkout requests, orders, and outbox events.
- One durable checkout queue moves accepted checkout work to workers.
- Workers use an atomic conditional inventory update inside a database transaction.
- The browser receives `202 Accepted` while the request is queued and later reads the result through polling or SSE.

Redis reduces pressure on the database, but PostgreSQL makes the final inventory decision. This prevents overselling even when requests are duplicated, queue messages are redelivered, or workers fail.

### Selected implementation stack

- Frontend: Vite + TypeScript with vanilla DOM APIs.
- API and workers: Go using the standard `net/http` server and `http.ServeMux`.
- PostgreSQL access: `pgx` with explicit SQL transactions and versioned migrations.
- Redis access: `go-redis` for Lua scripts, rate limiting, admission, caching, and fanout.
- Queue: RabbitMQ using a durable checkout queue with publisher confirms, manual acknowledgments, prefetch, retries, and dead-letter routing.
- Local infrastructure: Docker Compose.
- Production edge: CDN/WAF and an active-active load balancer in front of horizontally scaled Go replicas.

## 2. Requirements and Design Goals

### Functional requirements

1. Accept flash-sale traffic without silently losing valid requests.
2. Prevent duplicate checkout requests from creating duplicate orders.
3. Guarantee that no more than 500 smartphones are confirmed.
4. Provide a clear customer-visible result: confirmed, sold out, or failed.
5. Support retries after network failures and worker failures.
6. Keep checkout API replicas stateless so they can scale horizontally.

### Non-functional requirements

1. Handle more than 100,000 checkout requests in 10 seconds.
2. Protect the database from the full traffic spike.
3. Keep the request path short and avoid synchronous order processing.
4. Avoid row-lock contention and database timeouts where possible.
5. Keep frontend bundle size below 200 KB and target Time to Interactive below 1.5 seconds.
6. Provide observability for queue age, worker throughput, database locks, and confirmed inventory.

### Sale timing requirement

The sale opens at exactly **8:00 PM** during the festival. Sale timing is a server-side rule, not a browser-side assumption.

- The sale start timestamp is stored in the sale configuration.
- The server uses its own authoritative clock.
- Requests arriving before 8:00 PM receive `403 NOT_LIVE`.
- A request rejected as `NOT_LIVE` must not decrement Redis or database inventory.
- The browser displays a countdown synchronized to server time, but the countdown never decides whether checkout is allowed.
- At 8:00 PM, the admission or reservation operation checks the server time atomically before granting access.
- A user device with an incorrect clock cannot open the sale early.

## 3. Storage Decisions

### PostgreSQL: persistent business truth

PostgreSQL stores all durable business state:

- `inventory`: product stock and available quantity.
- `checkout_requests`: idempotency keys, tracking IDs, request fingerprints, state, and results.
- `orders`: confirmed order records.
- `outbox_events`: events that must be published after a committed database transaction.

The database is the authoritative source for whether a phone has been sold.

The `checkout_requests` table should include:

```sql
CREATE TABLE checkout_requests (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL,
    product_id UUID NOT NULL,
    idempotency_key UUID NOT NULL,
    request_hash TEXT NOT NULL,
    tracking_id UUID NOT NULL,
    status TEXT NOT NULL,
    processing_token UUID,
    lease_expires_at TIMESTAMPTZ,
    result JSONB,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (user_id, idempotency_key)
);
```

The unique constraint makes duplicate requests safe at the database level. If the same user sends the same key repeatedly, PostgreSQL returns the existing request record instead of creating another one.

### Redis Cluster: fast temporary coordination

Redis stores short-lived, high-volume data:

- Rate-limit counters.
- Admission-control tokens.
- Optional virtual waiting-room data.
- Cached checkout status.
- Live status fanout for SSE.

Redis is not the durable owner of inventory or orders. If Redis restarts, PostgreSQL still contains the correct order and inventory state. The cache can be rebuilt.

### Why not store idempotency only in Redis?

Redis is faster, but it is a poor sole authority for idempotency because keys can expire, replicas can fail over, and cached state can be lost. PostgreSQL provides a durable record and a unique constraint that can participate in the same transaction as the order and inventory decrement.

Redis may cache the result of an idempotency lookup, but a cache miss or Redis failure must fall back to PostgreSQL.

## 4. High-Level Architecture

```text
Browser
  |
  v
CDN / WAF
  |
  v
Active-active load balancer and API gateway fleet
  |
  v
Redis rate limit and admission control
  |
  v
Stateless checkout API replicas
  |
  +--> PostgreSQL checkout_requests + outbox_events
  |
  v
Outbox relay
  |
  v
Durable checkout queue
  |
  v
Bounded worker pool
  |
  v
PostgreSQL atomic inventory transaction
  |
  +--> orders and terminal checkout state
  |
  +--> status outbox event
              |
              v
      Redis status cache and SSE fanout
              |
              v
          Browser status display
```

There is one durable business queue. Redis admission control is a traffic-shaping mechanism, not a second source of truth for orders.

## 5. Sequential Request Flow

### Step 1: Browser loads the sale page

The browser requests HTML, CSS, JavaScript, images, and static product data from the CDN.

#### CDN cache hit

The CDN returns the asset immediately. The application servers and database are not involved.

#### CDN cache miss

The CDN fetches the asset from the origin, caches it, and returns it to the browser. Cache prewarming and long cache lifetimes reduce origin traffic at launch.

The frontend should be optimized for the assignment’s targets:

- Bundle below 200 KB.
- Time to Interactive below 1.5 seconds.

No inventory is reserved during page load.

Before the sale opens, the page may display the server-synchronized countdown. The checkout button stays disabled until the server reports that the sale is live. This is only a client-side usability measure; the server repeats the time check on every checkout request.

### Step 2: User clicks Buy

The browser generates one idempotency key before sending the request:

```javascript
const key = crypto.randomUUID();
sessionStorage.setItem("checkout:phone-1:key", key);
```

The button is disabled immediately, and the browser sends:

```http
POST /checkout
Idempotency-Key: 7d5b...
Content-Type: application/json
Authorization: Bearer ...

{
  "productId": "phone-1"
}
```

The key is sent in a header. A cookie can identify the user session, but it is not the authority for the checkout attempt.

#### Duplicate click

The disabled button prevents most duplicate clicks. Server-side idempotency still handles duplicates from scripts, browser retries, or concurrent requests.

#### Network timeout

The browser retries the same `POST` with the same idempotency key. The server returns the existing request instead of creating a second one.

#### Browser refresh

The browser restores the key and tracking ID from `sessionStorage` and resumes status lookup. It does not automatically create a new checkout.

### Step 3: Gateway and load balancer

The request enters an active-active gateway fleet distributed across availability zones.

The public load balancer uses health checks and least-request or least-connection routing to send traffic to healthy gateway instances.

The gateway performs only lightweight operations:

1. TLS handling.
2. Request-size and syntax validation.
3. Authentication.
4. WAF checks.
5. Rate limiting.
6. Admission control.
7. Routing to a healthy stateless API replica.

It does not check inventory or run order transactions.

#### Gateway scaling

There is no single gateway instance receiving all 100,000 requests. The gateway fleet scales horizontally, and the load balancer removes unhealthy instances automatically.

#### Gateway overload

If the edge is saturated, it returns a controlled response such as:

```http
429 Too Many Requests
Retry-After: 2
```

The practical interpretation of “0% dropped requests” is that accepted requests are not silently lost. A request is either controlled at the edge or durably accepted. Unlimited requests cannot all be processed immediately.

### Step 4: Redis rate limiting and admission control

The gateway performs an atomic Redis check before forwarding the request.

The admission script first checks the sale window against the Redis or application server clock. If the current time is earlier than the configured 8:00 PM start timestamp, it returns `NOT_LIVE` without changing stock, creating a claim, or enqueueing a job.

#### Rate limiting

Use a token bucket implemented with a Redis Lua script.

Example keys:

```text
rate:{userId}:checkout
rate:{ip}:checkout
rate:{session}:checkout
```

The Lua script reads the token count, calculates refill, removes one token if available, and returns either allow or deny. The operation is atomic.

A sorted set is not required for every rate limit. Use a sorted set only when an exact sliding time window is required. Token buckets use constant-size state and are cheaper for this traffic pattern.

#### Admission control

The sale admission gate limits how many requests can enter durable checkout processing at once. It may use a short-lived Redis counter or reservation token:

```text
sale:phone-1:admission:{requestId}
```

This is not an order and does not confirm inventory. It only protects the checkout API and database from the 100,000-request burst.

#### Redis failure

If Redis is unavailable, new admission should stop or be reduced to a tightly controlled fallback. Passing unlimited traffic directly to PostgreSQL would defeat the purpose of the admission layer.

### Step 5: Checkout API creates durable request state

An admitted request reaches any stateless checkout API replica.

The API calculates a request fingerprint from the authenticated user, product, and relevant checkout fields. It then performs an insert-or-read operation in PostgreSQL:

```sql
BEGIN;

INSERT INTO checkout_requests (
    id,
    user_id,
    product_id,
    idempotency_key,
    request_hash,
    tracking_id,
    status,
    created_at,
    updated_at
)
VALUES (
    :request_id,
    :user_id,
    :product_id,
    :idempotency_key,
    :request_hash,
    :tracking_id,
    'QUEUED',
    now(),
    now()
)
ON CONFLICT (user_id, idempotency_key) DO NOTHING;

SELECT *
FROM checkout_requests
WHERE user_id = :user_id
  AND idempotency_key = :idempotency_key;
```

The API also writes an outbox event for a newly created request before committing.

#### New key

The request becomes `QUEUED`. The API returns:

```http
202 Accepted
```

```json
{
  "trackingId": "T456",
  "status": "QUEUED",
  "statusUrl": "/checkout-status/T456"
}
```

This means the request was accepted for processing. It does not mean the phone is confirmed.

#### Existing key

The API returns the existing tracking ID and current state:

- `QUEUED`: still waiting.
- `PROCESSING`: a worker is handling it.
- `CONFIRMED`: return the stored order confirmation.
- `SOLD_OUT`: return the stored sold-out result.
- `FAILED`: return the stored permanent failure.

#### Conflicting reuse

If the same key is reused with a different request hash, return `409 Conflict`. One idempotency key represents one logical checkout attempt.

### Step 6: Transactional outbox

The API writes both the checkout request and its outbox event in one PostgreSQL transaction:

```sql
BEGIN;

-- Insert checkout_requests row if this is a new key.
-- Read existing row if the key already exists.

INSERT INTO outbox_events (
    id,
    event_type,
    aggregate_id,
    payload,
    status,
    created_at
)
VALUES (
    :event_id,
    'CHECKOUT_REQUESTED',
    :request_id,
    :payload,
    'PENDING',
    now()
);

COMMIT;
```

The API returns `202` only after this transaction commits.

#### What the outbox solves

Without an outbox, the database write could succeed while queue publication fails. The checkout would exist but no worker would know about it.

The outbox relay reads committed `PENDING` rows and publishes them to the durable queue.

#### Relay failure

If the relay fails before publication, the row remains `PENDING` and is retried.

If it publishes successfully and fails before marking the row `PUBLISHED`, it may publish a duplicate. This is expected and safe because workers are idempotent.

#### What the outbox does not solve

The outbox does not confirm inventory, guarantee exactly-once queue publication, or prevent duplicate queue delivery. It only keeps durable database state and eventual queue publication connected.

### Step 7: Durable checkout queue

The outbox relay publishes one checkout job:

```json
{
  "requestId": "R123",
  "trackingId": "T456",
  "idempotencyKey": "K789",
  "userId": "U1",
  "productId": "phone-1"
}
```

This is the only durable business queue.

The queue uses at-least-once delivery. A message can be delivered more than once, so duplicate delivery is a normal condition rather than an exceptional assumption.

The queue should provide:

- Durable messages.
- Acknowledgment support.
- Redelivery after worker failure.
- Retry limits.
- Dead-letter handling.
- Queue-depth and queue-age metrics.
- Partitioning or sharding if broker throughput requires it.

### Step 8: Worker claims processing ownership

The worker receives the job and creates a processing token.

```sql
UPDATE checkout_requests
SET
    status = 'PROCESSING',
    processing_token = :worker_token,
    lease_expires_at = now() + interval '30 seconds',
    updated_at = now()
WHERE id = :request_id
  AND (
      status = 'QUEUED'
      OR (
          status = 'PROCESSING'
          AND lease_expires_at < now()
      )
  )
RETURNING *;
```

If one row is returned, the worker owns the current processing lease.

If zero rows are returned, the worker stops because another worker owns the request, or the request is already terminal.

#### Worker crash before acknowledgment

If the worker crashes before acknowledging the queue message, the queue redelivers it.

The new worker checks PostgreSQL. If the request is already `CONFIRMED`, `SOLD_OUT`, or `FAILED`, it acknowledges the duplicate without doing the inventory operation again.

#### Why not depend on a Redis distributed lock?

A Redis lock can expire while a paused worker is still running. A second worker can then acquire the lock while the first worker continues. Redis failover creates additional ownership ambiguity.

The PostgreSQL lease is durable and can be checked together with the inventory transaction. A Redis lock can be an optional optimization, but it cannot be the correctness mechanism.

### Step 9: Worker scaling

Queue scaling and worker scaling solve different problems.

#### Queue scaling

Scale queue brokers or partitions when the broker cannot handle message throughput, replication, or storage requirements.

#### Worker scaling

Scale workers when queue age grows and the database can safely handle additional transactions.

Do not add unlimited workers. All workers for the same hot product eventually contend on the same inventory row. Beyond a certain point, more workers increase lock waiting rather than increasing confirmed orders.

Monitor:

- Queue depth.
- Oldest message age.
- Worker processing latency.
- Retry rate.
- Database connection utilization.
- Inventory-row lock wait time.
- Transaction latency.

### Step 10: Inventory confirmation transaction

The worker verifies its ownership and performs the final inventory operation in one PostgreSQL transaction:

```sql
BEGIN;

SELECT status, processing_token
FROM checkout_requests
WHERE id = :request_id
FOR UPDATE;

-- Verify status = PROCESSING.
-- Verify processing_token = this worker's token.

UPDATE inventory
SET available = available - 1
WHERE product_id = :product_id
  AND available > 0
RETURNING available;
```

#### Successful inventory update

If one row is returned:

```sql
INSERT INTO orders (
    id,
    checkout_request_id,
    user_id,
    product_id,
    status,
    created_at
)
VALUES (
    :order_id,
    :request_id,
    :user_id,
    :product_id,
    'CONFIRMED',
    now()
);

UPDATE checkout_requests
SET
    status = 'CONFIRMED',
    result = :confirmation,
    processing_token = NULL,
    lease_expires_at = NULL,
    updated_at = now()
WHERE id = :request_id;

INSERT INTO outbox_events (...)
VALUES (..., 'CHECKOUT_CONFIRMED', ...);

COMMIT;
```

#### No inventory available

If the conditional update affects zero rows:

```sql
UPDATE checkout_requests
SET
    status = 'SOLD_OUT',
    result = :sold_out_result,
    processing_token = NULL,
    lease_expires_at = NULL,
    updated_at = now()
WHERE id = :request_id;
```

No order is inserted.

#### Concurrent workers

At stock 500:

```text
Worker A: 500 → 499
Worker B: 499 → 498
```

At stock 1:

```text
Worker A: 1 → 0
Worker B: waits for the row lock, then sees 0
Worker B: zero rows affected → SOLD_OUT
```

At stock 0, all later updates affect zero rows.

The database uses the current committed row value. It does not allow two workers to read 500 and both overwrite it with 499. A sequence such as `500 → 499 → 498 → 497` is correct if three transactions committed.

#### Transaction failure

Deadlocks, serialization failures, and connection failures are retryable. The worker retries with the same request ID and idempotency key, first checking whether the earlier transaction already committed.

### Step 11: Status cache and notification

The confirmation transaction creates a status outbox event. The relay publishes it to the status layer.

Redis stores:

```text
checkout:status:{trackingId}
```

Example:

```json
{
  "status": "CONFIRMED",
  "version": 3,
  "orderId": "O123"
}
```

Statuses use monotonic versions:

```text
QUEUED = 1
PROCESSING = 2
CONFIRMED = 3
```

Redis accepts only a newer version, preventing delayed events from moving a request backward.

Active states have short TTLs. Terminal states have longer TTLs. PostgreSQL retains the durable result.

If Redis misses the status:

```text
Status API → Redis miss → PostgreSQL → Redis refill → Browser
```

Redis therefore absorbs the normal read load while PostgreSQL remains the recovery source.

### Step 12: Browser checks status

After receiving `202 Accepted`, the browser calls:

```http
GET /checkout-status/T456
```

It does not call this endpoint before the `POST /checkout` response provides a tracking ID.

The browser polls only while the status is non-terminal:

```text
QUEUED → PROCESSING → CONFIRMED
                    → SOLD_OUT
                    → FAILED
```

Polling uses exponential backoff. Once a terminal result is returned, polling stops.

The browser displays:

- Order confirmation for `CONFIRMED`.
- Inventory exhausted for `SOLD_OUT`.
- Retry or failure guidance for `FAILED`.

### Step 13: Optional SSE status delivery

SSE can replace polling but should be handled by a separate status service.

```text
Browser
  → SSE load balancer
  → SSE service replicas
  → Redis status cache and fanout
```

SSE can be overloaded by:

- Long-lived sockets.
- Per-connection memory.
- Heartbeats.
- Load-balancer connection limits.
- Reconnection storms.
- Event fanout volume.

The SSE service should:

- Open only after the browser receives a tracking ID.
- Send state changes rather than repeatedly querying PostgreSQL.
- Use heartbeats.
- Support `Last-Event-ID` for reconnects.
- Limit connection lifetime if needed.
- Fall back to polling under pressure.

Polling is the simpler baseline for this take-home. SSE is a separately scalable enhancement.

## 6. Failure Handling

| Failure | System behavior |
|---|---|
| CDN cache miss | CDN fetches from origin and caches the asset |
| Duplicate browser click | UI blocks it; server idempotency handles it anyway |
| Browser timeout | Retry using the same idempotency key |
| Gateway overload | Return controlled `429` or admission response |
| Rate limit exceeded | Reject before application processing |
| Redis unavailable | Stop or reduce admission to protect PostgreSQL |
| API replica failure | Load balancer routes to a healthy replica |
| PostgreSQL transaction failure | Retry safely using the same key |
| Outbox relay failure | Pending event remains in PostgreSQL and is retried |
| Duplicate outbox publication | Worker detects existing request state |
| Queue broker failure | Messages remain durable or are retried according to broker guarantees |
| Worker crash before acknowledgment | Queue redelivers the job |
| Worker crash after database commit | Redelivery sees the terminal state |
| Processing lease expires | Another worker can reclaim the request |
| Inventory reaches zero | Conditional update affects zero rows; result is `SOLD_OUT` |
| Redis status cache loss | Status API reads PostgreSQL and rebuilds the cache |
| SSE overload | Scale SSE service or fall back to polling |

## 7. Scaling Strategy for the 100,000-Request Burst

The traffic is deliberately absorbed in stages:

```text
Static content
  → CDN

Bad or excessive traffic
  → WAF and gateway rate limits

Excess legitimate traffic
  → Redis admission control

Accepted checkout requests
  → PostgreSQL request record and outbox

Durable business work
  → One checkout queue

Inventory processing
  → Bounded workers

Final inventory decision
  → PostgreSQL atomic update

Status reads
  → Redis cache

Status events
  → Redis fanout and optional SSE
```

The primary database should not process:

- Static asset requests.
- Every rejected request.
- Every rate-limit check.
- Every status poll.
- Every SSE heartbeat.

It should process durable business facts only:

- Accepted checkout requests.
- Outbox events.
- Inventory decrement attempts.
- Confirmed orders.
- Sold-out results.
- Permanent failure states.

## 8. Tradeoffs

### PostgreSQL versus Redis for inventory

Redis is faster for atomic counters, but PostgreSQL is the durable source of truth needed for orders, auditing, recovery, and transaction consistency. Redis can reduce traffic, but a Redis counter alone is not sufficient to prove durable ownership of inventory.

### Polling versus SSE

Polling is simpler and easier to scale with backoff. SSE gives faster one-way updates but consumes long-lived connections and requires a separate status service. Polling is the baseline; SSE is an optional enhancement.

### Database lease versus Redis lock

A PostgreSQL lease is durable and can be checked in the same transaction as inventory. A Redis lock may reduce duplicate work but has expiry and failover edge cases. The database lease is the correctness mechanism.

### Admission control versus accepting every request

Allowing all 100,000 requests directly into PostgreSQL creates unnecessary writes and contention. Redis admission control limits the amount of work that reaches the database. The tradeoff is that customers may receive a retry or waiting response before durable checkout acceptance.

### One queue versus multiple queues

One durable checkout queue keeps the business path clear and avoids confusion between temporary admission and durable work. Separate queues may be introduced later for unrelated work such as email, payment reconciliation, or fulfillment.

## 9. Testing and Acceptance Criteria

### Load tests

Test at least:

1. 100,000 checkout requests in 10 seconds.
2. 10,000 requests per second with duplicate clicks.
3. Many retries using the same idempotency key.
4. Concurrent requests when stock is 1.
5. Requests after stock reaches 0.
6. Worker crash before queue acknowledgment.
7. Worker crash after database commit.
8. Outbox relay crash before and after publication.
9. PostgreSQL deadlock or serialization retry.
10. Redis outage during admission.
11. Redis cache loss during status polling.
12. Queue backlog and worker autoscaling.
13. SSE reconnect storm.

### Acceptance criteria

The design passes when:

- Confirmed orders never exceed 500.
- Inventory never becomes negative.
- One idempotency key creates at most one order.
- Duplicate messages do not decrement inventory twice.
- Every durably accepted request reaches a terminal state.
- Queue age remains within the defined service objective.
- Worker concurrency does not create uncontrolled database lock contention.
- Status reads are served from Redis in normal operation.
- PostgreSQL remains the authoritative source after Redis failure.
- The frontend remains responsive during queueing and retries.

## 10. Final Design Decision

The recommended implementation is:

```text
CDN for static assets
→ active-active gateway fleet
→ Redis rate limiting and admission control
→ stateless checkout API
→ PostgreSQL checkout request + transactional outbox
→ one durable at-least-once checkout queue
→ bounded worker pool
→ PostgreSQL atomic inventory transaction
→ PostgreSQL order and terminal request state
→ Redis status cache and optional SSE fanout
→ browser polling or SSE result
```

This design handles the burst by controlling traffic before it reaches the database, handles retries through durable idempotency records, handles failures through the outbox and at-least-once processing, and guarantees zero overselling through the atomic conditional inventory update.

## 11. Alignment with the Assignment's Evaluation Framework

The assignment asks candidates to organize their answer into three areas. The design above follows the same framework.

### 11.1 Frontend Strategy: Client Optimization

#### Button throttling and debouncing

The Buy button is disabled on the first click. The client also keeps one in-flight checkout operation per product and idempotency key. This prevents repeated UI actions from generating identical payloads, while server-side idempotency protects against clients that bypass the UI.

#### CDN and asset prefetching

Static HTML, CSS, JavaScript, images, and product metadata are served through the CDN. Sale-critical assets can be preloaded or prefetched before the sale begins. Prefetching is limited to static or read-only data; it never reserves inventory or starts checkout.

This satisfies the assignment's goal of keeping the core application servers free for dynamic operations.

#### Local state and loading behavior

The frontend state machine is:

```text
IDLE → SUBMITTING → QUEUED → PROCESSING → CONFIRMED
                                      → SOLD_OUT
                                      → FAILED
```

The UI should show a clear queued or processing state and remain responsive. The browser stores the idempotency key and tracking ID so a refresh can resume the same request.

#### Polling, SSE, and WebSockets

Polling with exponential backoff is the baseline because it is simple and predictable. SSE is suitable for one-way sale-status events, but it requires a separately scalable connection service. WebSockets are unnecessary for this use case unless the product needs two-way real-time communication.

The status channel must not query PostgreSQL for every heartbeat or connected user. Redis status caching and event fanout absorb those reads.

#### Optimistic UI updates

The UI may optimistically display “request submitted” after the API returns `202 Accepted`. It must not optimistically display “purchase confirmed” or permanently decrement the visible inventory count before PostgreSQL confirms the order. The final purchase state comes from the server.

### 11.2 Backend and Middleware Architecture

#### Load balancing and stateless application clusters

An active-active gateway or AWS Application Load Balancer distributes requests across stateless Go API replicas. Health checks remove unhealthy replicas, and least-request or least-connection routing spreads work according to current load.

The Go request path must remain concurrency-safe and bounded:

- No unbounded goroutine or CPU-heavy work.
- No blocking filesystem operations.
- No long database transaction inside the HTTP request.
- No waiting for inventory confirmation before returning `202`.
- Bounded database connection pools.
- Asynchronous queue publication through the outbox relay.

The API replicas validate, admit, persist the request, and return. Workers perform the heavy database work later.

#### Distributed caching and Redis inventory tokens

Redis provides rate limits, admission tokens, cached status, and event fanout. A fast Redis token or reservation can reduce requests that reach the database. Redis Lua scripts are atomic and useful for this front-door coordination.

The Redis token is not the final inventory authority. PostgreSQL still performs the conditional decrement, so a Redis failover or reservation loss can cause temporary under-utilization but cannot create overselling.

#### Message queue and workers

The transactional outbox publishes accepted requests to one durable queue. Workers consume jobs asynchronously, claim processing ownership, and execute the inventory transaction. At-least-once delivery is expected, so duplicate messages are handled through the idempotency record and unique order constraint.

### 11.3 Database Consistency and Concurrency Control

#### Why `SELECT` followed by `UPDATE` is unsafe

This pattern has a race:

```sql
SELECT available FROM inventory WHERE product_id = :id;
-- application checks available > 0
UPDATE inventory SET available = available - 1
WHERE product_id = :id;
```

Two workers can read the same value before either update. One successful purchase can then be lost, or the application can confirm more orders than inventory allows.

#### Pessimistic locking

Pessimistic locking uses a row lock such as `SELECT ... FOR UPDATE` before changing the row.

Advantages:

- Makes ownership explicit.
- Useful when a transaction must inspect and update several related records.
- Prevents two transactions from modifying the same row simultaneously.

Tradeoffs:

- Workers wait on the hot inventory row.
- Long transactions increase lock duration.
- Too much concurrency can produce lock timeouts and reduced throughput.

#### Optimistic locking

Optimistic locking uses a version or conditional update and retries when another transaction has changed the row.

Advantages:

- Lower waiting when contention is low.
- Failed attempts can be retried without holding a lock through application logic.

Tradeoffs:

- Heavy contention can produce many retries.
- The application must handle conflicts correctly.
- It still needs a database constraint or atomic update to enforce the stock limit.

#### Selected approach

Use a short PostgreSQL transaction with a conditional atomic update:

```sql
UPDATE inventory
SET available = available - 1
WHERE product_id = :id
  AND available > 0;
```

This combines the stock check and decrement into one database operation. PostgreSQL internally protects the row while applying the update. The worker checks the affected-row count:

- One row affected: inventory was claimed.
- Zero rows affected: the item is sold out.

Redis Lua scripting provides a similar atomic operation for the fast admission or reservation layer, but PostgreSQL remains the durable correctness boundary.

## 12. Mapping to the Reference Solution Phases

### Phase A: Frontend Layer

1. Disable or throttle the Buy button on the first click.
2. Serve static assets through a CDN and prefetch sale-critical read-only assets.
3. Maintain local client state for submitting, queued, processing, and terminal results.
4. Use polling with backoff by default; use SSE as an optional status upgrade.
5. Use optimistic UI only for “request submitted,” never for confirmed inventory ownership.

### Phase B: Backend Infrastructure

1. Place an active-active load balancer or AWS Application Load Balancer in front of stateless API replicas.
2. Use Redis for rate limiting, admission tokens, cached status, and optional fast inventory reservations.
3. Use a durable asynchronous queue so database work is not performed inside the HTTP request-response cycle.
4. Scale workers based on queue age while capping concurrency at the database's safe capacity.
5. Use RabbitMQ as the durable work queue with publisher confirms, manual acknowledgments, prefetch backpressure, retries, and dead-letter routing. Redis remains the fast coordination layer rather than the queue durability boundary.

### Phase C: Data Integrity and Persistence

1. Use asynchronous workers to process accepted checkout jobs.
2. Execute a short ACID transaction for the inventory decrement and order creation.
3. Use `UPDATE ... WHERE available > 0` instead of application-level `SELECT` then `UPDATE` logic.
4. Use unique idempotency and order constraints to make retries and duplicate messages safe.
5. Publish terminal status through the outbox so cached status and live notifications follow committed database state.

## 13. Explicit Rubric Alignment

This design addresses the characteristics expected in the strongest grading tier:

- Identifies the database row-lock and hot-inventory bottleneck.
- Removes heavy database work from the synchronous API path.
- Uses a durable asynchronous queue and retry-safe workers.
- Uses Redis atomic operations for fast admission and coordination.
- Uses a PostgreSQL conditional update as the final inventory guard.
- Handles duplicate clicks, retries, duplicate queue delivery, and worker crashes.
- Uses a CDN and frontend optimization for bundle size and Time to Interactive.
- Uses stateless application replicas and horizontal load balancing.
- Explains the tradeoff between pessimistic locking, optimistic locking, Redis Lua operations, and durable SQL transactions.

The design also avoids the weaker patterns identified by the rubric: a monolithic synchronous checkout path, direct blocking database calls from the frontend request, application-level double counters, and a separate `SELECT` followed by an unsafe `UPDATE`.

## 14. Explicit PDF Requirement Checklist

| PDF detail | Covered in this design |
|---|---|
| Sale opens exactly at 8:00 PM | Server-authoritative sale-window check and synchronized client countdown |
| 500 physical devices | Inventory invariant and conditional decrement |
| More than 100,000 requests in the first 10 seconds | Layered CDN, gateway, Redis admission, queue, and bounded workers |
| Previous oversell of 540 checkouts | Atomic inventory update and duplicate-safe processing |
| Frontend bundle below 200 KB | CDN, prefetching, lightweight client, and explicit target |
| Time to Interactive below 1.5 seconds | Explicit frontend performance target |
| Prevent repeated API payloads | Button lock, idempotency key, and server-side unique constraint |
| Graceful loading and failed queues | `202`, queued state, polling/SSE, retry, and terminal states |
| Rate-limit bad actors | Gateway and Redis token-bucket limits |
| 0% dropped requests target | Controlled admission, durable acceptance, retries, and explicit load tests |
| Stateless Node/Go-style application clusters | Stateless replicas, event-loop-safe request path, horizontal scaling |
| CDN edge delivery | Static asset caching, prewarming, and origin protection |
| Distributed cache / Redis inventory tokens | Redis admission, rate limits, cache, and optional reservations |
| Kafka/RabbitMQ-style asynchronous queue discussion | Queue technology tradeoff and consumer recovery requirements |
| ACID database transaction | Inventory, order, request state, and status event transaction |
| Atomic decrement | `UPDATE ... WHERE available > 0` and affected-row check |
| Pessimistic versus optimistic locking | Explicit comparison and selected conditional-update design |
| Redis atomic/Lua alternative | Lua token-bucket and admission operations |
| Frontend polling versus WebSockets/SSE | Polling baseline, SSE enhancement, WebSocket tradeoff |
| Optimistic UI updates | Only request-submitted state is optimistic; confirmation is server-authoritative |
| Frontend, backend, and database evaluation sections | Explicit alignment sections and Phase A/B/C mapping |

