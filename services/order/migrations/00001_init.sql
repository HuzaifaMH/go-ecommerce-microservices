-- +goose Up
CREATE TABLE orders (
    id              TEXT PRIMARY KEY,
    customer_id     TEXT        NOT NULL,
    idempotency_key TEXT        NOT NULL,
    currency_code   CHAR(3)     NOT NULL,
    total_minor     BIGINT      NOT NULL CHECK (total_minor > 0),
    status          TEXT        NOT NULL CHECK (status IN ('pending', 'stock_reserved', 'confirmed', 'cancelled')),
    cancel_reason   TEXT        NOT NULL DEFAULT '',
    -- Set when a payment succeeded for an order that was already cancelled.
    refund_required BOOLEAN     NOT NULL DEFAULT false,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Makes CreateOrder idempotent: a customer's key maps to one order.
    UNIQUE (customer_id, idempotency_key),
    CHECK ((status = 'cancelled') = (cancel_reason <> ''))
);

CREATE INDEX orders_list_idx          ON orders (created_at DESC, id DESC);
CREATE INDEX orders_customer_list_idx ON orders (customer_id, created_at DESC, id DESC);
-- The saga timeout sweeper only looks at unfinished orders.
CREATE INDEX orders_unfinished_idx    ON orders (updated_at) WHERE status IN ('pending', 'stock_reserved');

CREATE TABLE order_items (
    order_id         TEXT    NOT NULL REFERENCES orders (id),
    sku              TEXT    NOT NULL,
    quantity         INTEGER NOT NULL CHECK (quantity > 0),
    unit_price_minor BIGINT  NOT NULL CHECK (unit_price_minor >= 0),
    PRIMARY KEY (order_id, sku)
);

-- Transactional outbox and inbox (see pkg/outbox/pgstore.Schema).
CREATE TABLE outbox (
    id           TEXT PRIMARY KEY,
    subject      TEXT        NOT NULL,
    payload      BYTEA       NOT NULL,
    headers      JSONB       NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ
);
CREATE INDEX outbox_pending_idx ON outbox (created_at, id) WHERE published_at IS NULL;

CREATE TABLE inbox (
    consumer     TEXT        NOT NULL,
    message_id   TEXT        NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, message_id)
);

-- +goose Down
DROP TABLE inbox;
DROP TABLE outbox;
DROP TABLE order_items;
DROP TABLE orders;
