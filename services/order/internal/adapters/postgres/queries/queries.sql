-- name: InsertOrder :one
-- Returns no row when the customer already used this idempotency key; the caller then reads the existing order.
INSERT INTO orders (id, customer_id, idempotency_key, currency_code, total_minor, status, cancel_reason, refund_required, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (customer_id, idempotency_key) DO NOTHING
RETURNING id, customer_id, idempotency_key, currency_code, total_minor, status, cancel_reason, refund_required, created_at, updated_at;

-- name: InsertOrderItem :exec
INSERT INTO order_items (order_id, sku, quantity, unit_price_minor) VALUES ($1, $2, $3, $4);

-- name: GetOrder :one
SELECT id, customer_id, idempotency_key, currency_code, total_minor, status, cancel_reason, refund_required, created_at, updated_at
FROM orders WHERE id = $1;

-- name: GetOrderForUpdate :one
SELECT id, customer_id, idempotency_key, currency_code, total_minor, status, cancel_reason, refund_required, created_at, updated_at
FROM orders WHERE id = $1 FOR UPDATE;

-- name: GetOrderByKey :one
SELECT id, customer_id, idempotency_key, currency_code, total_minor, status, cancel_reason, refund_required, created_at, updated_at
FROM orders WHERE customer_id = $1 AND idempotency_key = $2;

-- name: GetItemsForOrders :many
SELECT order_id, sku, quantity, unit_price_minor
FROM order_items
WHERE order_id = ANY($1::text[])
ORDER BY order_id, sku;

-- name: UpdateOrder :exec
UPDATE orders
SET status = $2, cancel_reason = $3, refund_required = $4, updated_at = $5
WHERE id = $1;

-- name: ListOrdersFirstPage :many
SELECT id, customer_id, idempotency_key, currency_code, total_minor, status, cancel_reason, refund_required, created_at, updated_at
FROM orders
WHERE (sqlc.arg(customer_id)::text = '' OR customer_id = sqlc.arg(customer_id)::text)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListOrdersAfter :many
SELECT id, customer_id, idempotency_key, currency_code, total_minor, status, cancel_reason, refund_required, created_at, updated_at
FROM orders
WHERE (sqlc.arg(customer_id)::text = '' OR customer_id = sqlc.arg(customer_id)::text)
  AND (created_at, id) < (sqlc.arg(after_created_at)::timestamptz, sqlc.arg(after_id)::text)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: LockStaleOrders :many
-- Unfinished orders nobody has updated since the cutoff. SKIP LOCKED lets several sweepers run side by side.
SELECT id, customer_id, idempotency_key, currency_code, total_minor, status, cancel_reason, refund_required, created_at, updated_at
FROM orders
WHERE status IN ('pending', 'stock_reserved') AND updated_at < $1
ORDER BY updated_at, id
LIMIT $2
FOR UPDATE SKIP LOCKED;
