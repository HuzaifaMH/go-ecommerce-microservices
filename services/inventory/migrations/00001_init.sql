-- +goose Up
CREATE TABLE stock_items (
    sku              TEXT PRIMARY KEY,
    name             TEXT        NOT NULL,
    currency_code    CHAR(3)     NOT NULL,
    unit_price_minor BIGINT      NOT NULL CHECK (unit_price_minor >= 0),
    on_hand          INTEGER     NOT NULL CHECK (on_hand >= 0),
    reserved         INTEGER     NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    CHECK (reserved <= on_hand)
);

CREATE TABLE reservations (
    order_id    TEXT PRIMARY KEY,
    status      TEXT        NOT NULL CHECK (status IN ('active', 'released')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at TIMESTAMPTZ
);

CREATE TABLE reservation_lines (
    order_id TEXT    NOT NULL REFERENCES reservations (order_id),
    sku      TEXT    NOT NULL REFERENCES stock_items (sku),
    quantity INTEGER NOT NULL CHECK (quantity > 0),
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
DROP TABLE reservation_lines;
DROP TABLE reservations;
DROP TABLE stock_items;
