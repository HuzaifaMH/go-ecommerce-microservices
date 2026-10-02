-- name: GetPaymentByOrder :one
SELECT id, order_id, customer_id, currency_code, amount_minor, status, failure_reason, provider_ref, created_at
FROM payments
WHERE order_id = $1;

-- name: InsertPayment :one
-- Returns no row when the order already has a payment; the caller then reads the existing one.
INSERT INTO payments (id, order_id, customer_id, currency_code, amount_minor, status, failure_reason, provider_ref)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (order_id) DO NOTHING
RETURNING id, order_id, customer_id, currency_code, amount_minor, status, failure_reason, provider_ref, created_at;
