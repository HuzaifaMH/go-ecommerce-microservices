-- +goose Up
CREATE TABLE payments (
    id             TEXT PRIMARY KEY,
    -- One payment per order: this constraint is what makes charging idempotent.
    order_id       TEXT        NOT NULL UNIQUE,
    customer_id    TEXT        NOT NULL,
    currency_code  CHAR(3)     NOT NULL,
    amount_minor   BIGINT      NOT NULL CHECK (amount_minor > 0),
    status         TEXT        NOT NULL CHECK (status IN ('succeeded', 'failed')),
    failure_reason TEXT        NOT NULL DEFAULT '',
    provider_ref   TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((status = 'failed') = (failure_reason <> ''))
);

-- Transactional outbox (see pkg/outbox/pgstore.Schema). Charging is idempotent
-- by order, so this service needs no inbox.
CREATE TABLE outbox (
    id           TEXT PRIMARY KEY,
    subject      TEXT        NOT NULL,
    payload      BYTEA       NOT NULL,
    headers      JSONB       NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ
);
CREATE INDEX outbox_pending_idx ON outbox (created_at, id) WHERE published_at IS NULL;

-- +goose Down
DROP TABLE outbox;
DROP TABLE payments;
