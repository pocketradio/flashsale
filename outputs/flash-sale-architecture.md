# Flash Sale Architecture

This design handles a launch spike of more than 100,000 checkout requests for 500 phones while preserving one hard invariant:

> At most 500 orders can reach `CONFIRMED`.

The browser, gateway, queue, Redis, workers, and database each have a different job. The database transaction is the final authority for durable inventory ownership.

## 1. End-to-end request flow

```mermaid
flowchart TD
    U[Customer browser]
    UI[Checkout UI\nDisable button immediately\nGenerate/reuse idempotency key]
    CDN[CDN\nStatic HTML/CSS/JS/images]
    EDGE[API gateway / load balancer\nTLS, auth, rate limit, request IDs]
    WAIT[Waiting room / bounded admission queue\nAbsorb launch spike]
    API[Stateless checkout API replicas]
    IDEM[(Idempotency store\nkey -> request/result)]
    Q[(Durable checkout queue)]
    W[Checkout workers\nRetry-safe and idempotent]
    REDIS[(Redis\nFast admission counter / status cache)]
    DB[(Primary database\nACID inventory + orders)]
    STATUS[Status API / SSE endpoint]
    RESULT[Customer sees\nconfirmed, sold out, or retryable failure]

    U --> UI
    UI -. loads assets .-> CDN
    UI -->|POST /checkout\nIdempotency-Key: K| EDGE
    EDGE --> WAIT
    WAIT --> API
    API -->|check or create K| IDEM
    API -->|enqueue once| Q
    API -->|202 Accepted + tracking ID| UI
    Q --> W
    W -->|optional fast gate / cache| REDIS
    W -->|atomic conditional decrement\ninside transaction| DB
    W -->|persist final outcome| IDEM
    UI -->|GET /checkout-status/:id\nwith backoff| STATUS
    STATUS --> IDEM
    STATUS --> DB
    STATUS --> RESULT
```

### Meaning of `202 Accepted`

`202 Accepted` means the request was accepted for processing. It does not mean the phone has been purchased. The final result is returned through the status endpoint or an SSE update.

## 2. Component responsibilities

```mermaid
flowchart LR
    subgraph Client[Client layer]
        B[Checkout button]
        K[Idempotency key\ncreated before first submit]
        S[Local state\nsubmitting / queued / done]
        P[Polling with exponential backoff\nor SSE subscription]
    end

    subgraph Edge[Traffic layer]
        CDN2[CDN]
        LB[Load balancer]
        RL[Rate limiter]
        WR[Waiting room]
    end

    subgraph App[Application layer]
        C[Stateless checkout API]
        ST[Status API]
        Q2[Durable queue]
        WK[Worker pool]
    end

    subgraph Data[Data layer]
        IS[Idempotency records]
        RC[Redis cache / admission tokens]
        INV[(Inventory row)]
        ORD[(Orders table)]
    end

    B --> K --> C
    B --> S
    S --> P --> ST
    CDN2 -. static assets .-> B
    C --> LB --> RL --> WR
    WR --> C
    C --> IS
    C --> Q2
    Q2 --> WK
    WK --> RC
    WK --> INV
    WK --> ORD
    ST --> IS
    ST --> ORD
```

## 3. The idempotency-key path

The key prevents duplicate orders when the customer double-clicks, the browser retries, or the client times out after the server has already accepted the request.

```mermaid
sequenceDiagram
    autonumber
    participant B as Browser
    participant G as Gateway/API
    participant I as Idempotency store
    participant Q as Durable queue
    participant W as Worker
    participant D as Database

    Note over B: Create K once for this checkout attempt
    B->>G: POST /checkout\nIdempotency-Key: K
    G->>I: Look up K

    alt K already has final result
        I-->>G: CONFIRMED or SOLD_OUT result
        G-->>B: Return the stored result
    else K exists but is still queued
        I-->>G: QUEUED + tracking ID
        G-->>B: 202 Accepted, same tracking ID
    else K is new
        I->>I: Atomically create K as QUEUED
        G->>Q: Publish checkout job(K, customer, product)
        G-->>B: 202 Accepted + tracking ID
    end

    Q->>W: Deliver job(K)
    W->>I: Claim processing for K

    alt K was already completed by an earlier delivery
        I-->>W: Already completed
        W-->>Q: Acknowledge duplicate delivery
    else First processing attempt
        W->>D: Begin transaction
        W->>D: UPDATE inventory\nSET stock = stock - 1\nWHERE product_id = P\nAND stock > 0

        alt Exactly one row updated
            W->>D: INSERT order with unique K
            W->>D: Commit transaction
            W->>I: Store CONFIRMED result
        else Zero rows updated
            W->>D: Roll back / no order
            W->>I: Store SOLD_OUT result
        end
        W-->>Q: Acknowledge job
    end
```

## 4. Inventory correctness transaction

The dangerous version is:

```sql
SELECT stock FROM inventory WHERE product_id = :product_id;
-- application checks stock > 0
UPDATE inventory SET stock = stock - 1 WHERE product_id = :product_id;
```

Two workers can read the same stock value before either update. The safe version makes the check and decrement one database operation.

```mermaid
sequenceDiagram
    participant W1 as Worker 1
    participant W2 as Worker 2
    participant DB as Database

    par Concurrent attempts
        W1->>DB: UPDATE inventory\nSET stock = stock - 1\nWHERE product_id = P AND stock > 0
        W2->>DB: UPDATE inventory\nSET stock = stock - 1\nWHERE product_id = P AND stock > 0
    end

    DB-->>W1: One row updated -> success
    DB-->>W2: One row updated or zero rows\n(depending on remaining stock)
    Note over DB: The database serializes the conflicting row update.
    Note over DB: Once stock reaches zero, later updates affect zero rows.
```

The worker must check the affected-row count. A successful SQL statement is not enough: `rows_affected = 1` means inventory was claimed; `rows_affected = 0` means sold out.

## 5. Transaction boundary

```mermaid
flowchart TD
    A[Worker receives job with key K]
    B{Has K already completed?}
    C[Begin database transaction]
    D[Conditional inventory decrement]
    E{rows affected = 1?}
    F[Insert order\nunique idempotency key]
    G[Commit transaction]
    H[Mark K CONFIRMED]
    I[Rollback / no order]
    J[Mark K SOLD_OUT]
    R[Retry with backoff\nif transient database failure]

    A --> B
    B -- yes --> H
    B -- no --> C
    C --> D --> E
    E -- yes --> F --> G --> H
    E -- no --> I --> J
    C -. deadlock / timeout / unavailable .-> R
    R -. retry same K .-> B
```

The order insert should have a unique constraint on the idempotency key. That database constraint is a second line of defense against duplicate message delivery.

## 6. Order state machine

```mermaid
stateDiagram-v2
    [*] --> RECEIVED: POST /checkout
    RECEIVED --> QUEUED: idempotency key created\nand job durably enqueued
    RECEIVED --> EXISTING: key already known
    EXISTING --> QUEUED: prior request still processing
    EXISTING --> CONFIRMED: prior result was success
    EXISTING --> SOLD_OUT: prior result was sold out
    QUEUED --> PROCESSING: worker claims job
    PROCESSING --> CONFIRMED: atomic decrement succeeds\nand order transaction commits
    PROCESSING --> SOLD_OUT: atomic decrement affects 0 rows
    PROCESSING --> RETRYABLE_FAILURE: transient dependency failure
    RETRYABLE_FAILURE --> QUEUED: retry same key
    PROCESSING --> FAILED: permanent validation or business failure
    CONFIRMED --> [*]
    SOLD_OUT --> [*]
    FAILED --> [*]
```

## 7. Failure and retry behavior

```mermaid
flowchart TD
    X[Failure occurs]
    T{Transient?}
    D{Did the database transaction commit?}
    Y[Retry the same job and same idempotency key]
    N[Move to dead-letter queue\nand mark FAILED]
    L[Worker checks idempotency record\nor unique key before doing work]
    C[Return the already stored result\nwithout decrementing again]

    X --> T
    T -- no --> N
    T -- yes --> D
    D -- unknown / timeout --> L
    L --> C
    D -- definitely not committed --> Y
    Y --> L
```

Examples of transient failures: database connection reset, queue delivery timeout, or a worker crash. The safe retry rule is always: reuse the same idempotency key and make the worker check whether the operation already completed.

## 8. Polling or SSE status updates

```mermaid
sequenceDiagram
    participant UI as Browser UI
    participant API as Status API
    participant Store as Idempotency/order store

    alt Polling
        loop Until terminal state
            UI->>API: GET /checkout-status/:tracking_id
            API->>Store: Read state
            Store-->>API: QUEUED / PROCESSING / terminal state
            API-->>UI: State + retry-after hint
            UI->>UI: Wait with exponential backoff
        end
    else Server-sent events
        UI->>API: GET /checkout-status/:tracking_id\nAccept: text/event-stream
        API->>Store: Subscribe/read state changes
        Store-->>API: State transition
        API-->>UI: SSE event: confirmed / sold_out / failed
    end
```

Polling is simpler to implement and sufficient for a take-home. SSE is a good upgrade when the product wants near-real-time one-way updates. WebSockets are useful when the connection must support two-way interaction, but they add more connection-management complexity.

## 9. Decisions to state explicitly in the submission

1. The database is the durable source of truth for confirmed inventory.
2. Redis may reduce admission pressure or provide fast temporary state, but Redis alone does not prove durable order ownership.
3. Every checkout carries an idempotency key, and the key has a unique database constraint.
4. The checkout API returns `202 Accepted` while work is queued.
5. The worker uses one atomic conditional decrement and checks the affected-row count.
6. Queue delivery is treated as at-least-once, so duplicate messages are expected and safe.
7. The browser disables repeated submission and reuses the same key on retry.
8. A waiting room or bounded queue protects the database from the 100,000-request burst.
9. Rate limiting protects against abusive clients; it does not provide inventory correctness.
10. Load tests must verify that no more than 500 orders become `CONFIRMED` under concurrency.

## 10. Metrics and acceptance checks

```mermaid
flowchart LR
    TRAFFIC[Load test\n100,000+ requests / 10 sec]
    METRICS[Collect metrics]
    CHECKS[Acceptance checks]

    TRAFFIC --> METRICS
    METRICS --> A[API latency and error rate]
    METRICS --> B[Queue depth and age]
    METRICS --> C[Worker throughput and retries]
    METRICS --> D[Database lock time and transaction latency]
    METRICS --> E[Confirmed / sold-out counts]
    METRICS --> F[Frontend bundle size and TTI]

    CHECKS --> G[Confirmed orders <= 500]
    CHECKS --> H[No duplicate order per key]
    CHECKS --> I[No negative inventory]
    CHECKS --> J[Retries do not decrement twice]
    CHECKS --> K[Every accepted request reaches a terminal state]
    CHECKS --> L[UI remains responsive during queueing]
```

The PDF's frontend targets are a bundle below 200 KB and Time to Interactive below 1.5 seconds. Validate those with the chosen build and deployment setup. Validate backend targets with a realistic burst test rather than relying on a static capacity estimate.

