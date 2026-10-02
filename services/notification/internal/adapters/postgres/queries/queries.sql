-- name: InsertNotification :one
-- Returns no row when this (order, kind, channel) was already recorded; the caller then reads the existing one.
INSERT INTO notifications (id, order_id, customer_id, kind, channel, recipient, subject, body, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (order_id, kind, channel) DO NOTHING
RETURNING id, order_id, customer_id, kind, channel, recipient, subject, body, status, failure_reason, created_at, updated_at;

-- name: GetNotificationByKey :one
SELECT id, order_id, customer_id, kind, channel, recipient, subject, body, status, failure_reason, created_at, updated_at
FROM notifications
WHERE order_id = $1 AND kind = $2 AND channel = $3;

-- name: MarkNotificationSent :exec
-- Only a pending notification can become sent; final states never change.
UPDATE notifications SET status = 'sent', updated_at = $2 WHERE id = $1 AND status = 'pending';

-- name: MarkNotificationFailed :exec
UPDATE notifications SET status = 'failed', failure_reason = $2, updated_at = $3 WHERE id = $1 AND status = 'pending';

-- name: ListNotificationsFirstPage :many
SELECT id, order_id, customer_id, kind, channel, recipient, subject, body, status, failure_reason, created_at, updated_at
FROM notifications
WHERE (sqlc.arg(order_id)::text = '' OR order_id = sqlc.arg(order_id)::text)
  AND (sqlc.arg(customer_id)::text = '' OR customer_id = sqlc.arg(customer_id)::text)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListNotificationsAfter :many
SELECT id, order_id, customer_id, kind, channel, recipient, subject, body, status, failure_reason, created_at, updated_at
FROM notifications
WHERE (sqlc.arg(order_id)::text = '' OR order_id = sqlc.arg(order_id)::text)
  AND (sqlc.arg(customer_id)::text = '' OR customer_id = sqlc.arg(customer_id)::text)
  AND (created_at, id) < (sqlc.arg(after_created_at)::timestamptz, sqlc.arg(after_id)::text)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);
