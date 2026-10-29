# Flash Sale

Go `net/http` API, PostgreSQL ACID settlement, Redis cache/admission, RabbitMQ durable work queue, NGINX edge, and a Vite-ready static frontend. The implementation follows `outputs/flash-sale-spec-checklist.md` and the PDF's 500-unit/8:00 PM/100k-request constraints.

## Run

```sh
docker compose up --build
```

The local edge is `http://localhost:8080`. The API returns `403 NOT_LIVE` until `SALE_START`; set it to an ISO timestamp for a test window. `POST /api/v1/checkout` requires `X-User-ID`, `Idempotency-Key`, and `{"product_id":"phone-500","quantity":1}`.

## Verification

```sh
go test ./...
```

The database migration enforces non-negative stock, one order per checkout request, and durable idempotency. The worker acknowledges RabbitMQ only after the terminal PostgreSQL transaction commits.
