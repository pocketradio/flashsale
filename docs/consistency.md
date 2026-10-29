# Inventory consistency

Inventory settlement is decided by PostgreSQL, not by the client, Redis, or RabbitMQ. The worker executes `UPDATE inventory SET available = available - 1 WHERE product_id = $1 AND available >= 1` inside the same transaction as the order insert and terminal checkout state update. The affected-row count determines confirmation versus `SOLD_OUT`; the non-negative check and unique checkout-request/order constraints make duplicate delivery safe.
