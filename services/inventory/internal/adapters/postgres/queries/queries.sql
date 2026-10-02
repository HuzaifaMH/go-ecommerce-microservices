-- name: GetItem :one
SELECT sku, name, currency_code, unit_price_minor, on_hand, reserved
FROM stock_items
WHERE sku = $1;

-- name: ListItems :many
SELECT sku, name, currency_code, unit_price_minor, on_hand, reserved
FROM stock_items
WHERE sku > $1
ORDER BY sku
LIMIT $2;

-- name: LockItems :many
-- Rows are locked in SKU order so concurrent reservations cannot deadlock.
SELECT sku, name, currency_code, unit_price_minor, on_hand, reserved
FROM stock_items
WHERE sku = ANY($1::text[])
ORDER BY sku
FOR UPDATE;

-- name: SetReserved :exec
UPDATE stock_items SET reserved = $2 WHERE sku = $1;

-- name: UpsertItem :exec
INSERT INTO stock_items (sku, name, currency_code, unit_price_minor, on_hand)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (sku) DO NOTHING;

-- name: GetReservation :one
SELECT order_id, status FROM reservations WHERE order_id = $1;

-- name: GetReservationLines :many
SELECT sku, quantity FROM reservation_lines WHERE order_id = $1 ORDER BY sku;

-- name: CreateReservation :exec
INSERT INTO reservations (order_id, status) VALUES ($1, $2);

-- name: CreateReservationLine :exec
INSERT INTO reservation_lines (order_id, sku, quantity) VALUES ($1, $2, $3);

-- name: SetReservationStatus :exec
UPDATE reservations
SET status = $2,
    released_at = CASE WHEN $2 = 'released' THEN now() ELSE released_at END
WHERE order_id = $1;
