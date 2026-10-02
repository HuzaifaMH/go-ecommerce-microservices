-- +goose Up
CREATE TABLE notifications (
    id             TEXT PRIMARY KEY,
    order_id       TEXT        NOT NULL,
    customer_id    TEXT        NOT NULL,
    kind           TEXT        NOT NULL CHECK (kind IN ('order_confirmed', 'order_cancelled')),
    channel        TEXT        NOT NULL CHECK (channel IN ('email', 'sms')),
    recipient      TEXT        NOT NULL,
    subject        TEXT        NOT NULL,
    body           TEXT        NOT NULL,
    status         TEXT        NOT NULL CHECK (status IN ('pending', 'sent', 'failed')),
    failure_reason TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- One notification per order, kind and channel: this is what stops a
    -- redelivered order event from notifying the customer twice.
    UNIQUE (order_id, kind, channel),
    CHECK ((status = 'failed') = (failure_reason <> ''))
);

CREATE INDEX notifications_list_idx          ON notifications (created_at DESC, id DESC);
CREATE INDEX notifications_customer_list_idx ON notifications (customer_id, created_at DESC, id DESC);

-- +goose Down
DROP TABLE notifications;
