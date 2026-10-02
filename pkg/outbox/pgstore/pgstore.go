// Package pgstore is the PostgreSQL implementation of the transactional
// outbox (outbox.Store) and inbox (messaging.Inbox).
//
// Typical use inside an application service:
//
//	err := store.WithTx(ctx, func(ctx context.Context) error {
//	    if err := repo.Save(ctx, order); err != nil { return err } // repo uses store.Querier(ctx)
//	    return store.Enqueue(ctx, messaging.Message{Subject: "inventory.cmd.reserve", Data: payload})
//	})
package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/outbox"
)

// Schema creates the outbox and inbox tables. Each service copies it into its
// own goose migration; it is exported so integration tests use the same DDL.
const Schema = `
CREATE TABLE IF NOT EXISTS outbox (
    id           TEXT PRIMARY KEY,
    subject      TEXT        NOT NULL,
    payload      BYTEA       NOT NULL,
    headers      JSONB       NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS outbox_pending_idx ON outbox (created_at, id) WHERE published_at IS NULL;

CREATE TABLE IF NOT EXISTS inbox (
    consumer     TEXT        NOT NULL,
    message_id   TEXT        NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, message_id)
);
`

// ErrNoTx is returned by Enqueue when called outside WithTx.
var ErrNoTx = errors.New("pgstore: Enqueue must be called inside WithTx")

type txKey struct{}

// Querier is the subset of pgx shared by *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var (
	_ outbox.Store    = (*Store)(nil)
	_ messaging.Inbox = (*Store)(nil)
)

// Store implements the outbox and inbox on a pgx pool.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Querier returns the transaction carried by ctx, or the pool when there is none.
// Repositories use it so their writes join the surrounding WithTx transaction.
func (s *Store) Querier(ctx context.Context) Querier {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return tx
	}
	return s.pool
}

// WithTx runs fn in a transaction, committing if it returns nil and rolling
// back otherwise. Nested calls join the outer transaction.
func (s *Store) WithTx(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	if _, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return fn(ctx)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			// Use a fresh context so a cancelled ctx cannot leave the tx open.
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	if err = fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// Enqueue adds a message to the outbox as part of the current transaction.
// It generates a message ID when m.ID is empty and returns it.
func (s *Store) Enqueue(ctx context.Context, m messaging.Message) (string, error) {
	if _, ok := ctx.Value(txKey{}).(pgx.Tx); !ok {
		return "", ErrNoTx
	}
	if m.ID == "" {
		m.ID = uuid.NewString()
	}
	headers, err := json.Marshal(m.Headers)
	if err != nil {
		return "", fmt.Errorf("marshal headers: %w", err)
	}
	if m.Headers == nil {
		headers = []byte("{}")
	}
	_, err = s.Querier(ctx).Exec(ctx,
		`INSERT INTO outbox (id, subject, payload, headers) VALUES ($1, $2, $3, $4)`,
		m.ID, m.Subject, m.Data, headers)
	if err != nil {
		return "", fmt.Errorf("insert outbox: %w", err)
	}
	return m.ID, nil
}

// Dispatch implements outbox.Store. Rows are claimed with FOR UPDATE SKIP
// LOCKED, so several relay replicas can run safely without publishing the
// same row twice. Strict ordering is only guaranteed with a single relay.
func (s *Store) Dispatch(ctx context.Context, limit int, publish func(context.Context, outbox.Record) error) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // no-op after commit

	rows, err := tx.Query(ctx, `
		SELECT id, subject, payload, headers
		FROM outbox
		WHERE published_at IS NULL
		ORDER BY created_at, id
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return 0, fmt.Errorf("select outbox: %w", err)
	}
	var pending []outbox.Record
	for rows.Next() {
		var (
			rec     outbox.Record
			headers []byte
		)
		if err := rows.Scan(&rec.ID, &rec.Subject, &rec.Payload, &headers); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan outbox: %w", err)
		}
		if err := json.Unmarshal(headers, &rec.Headers); err != nil {
			rows.Close()
			return 0, fmt.Errorf("decode headers of %s: %w", rec.ID, err)
		}
		pending = append(pending, rec)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read outbox: %w", err)
	}

	var (
		done   []string
		pubErr error
	)
	for _, rec := range pending {
		if pubErr = publish(ctx, rec); pubErr != nil {
			break
		}
		done = append(done, rec.ID)
	}

	if len(done) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = ANY($1)`, done); err != nil {
			return 0, fmt.Errorf("mark published: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, fmt.Errorf("commit dispatch: %w", err)
		}
	}
	return len(done), pubErr
}

// Once implements messaging.Inbox. The inbox row and fn's writes commit
// together, so a failure in fn leaves no trace and the message can be retried.
func (s *Store) Once(ctx context.Context, consumer, messageID string, fn func(ctx context.Context) error) error {
	if messageID == "" {
		return fmt.Errorf("inbox: %w", messaging.ErrMissingID)
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		tag, err := s.Querier(ctx).Exec(ctx,
			`INSERT INTO inbox (consumer, message_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			consumer, messageID)
		if err != nil {
			return fmt.Errorf("insert inbox: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil // already processed
		}
		return fn(ctx)
	})
}
