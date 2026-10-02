// Package outbox implements the relay half of the transactional outbox pattern.
//
// Services write domain state and an outbox row in one database transaction,
// so the two cannot diverge. The Relay then publishes pending rows to the
// broker. Delivery is at-least-once; the row ID is used as the message ID so
// JetStream de-duplicates retries, and consumers use an inbox for idempotency.
package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
)

// Record is a pending outbox row.
type Record struct {
	ID      string
	Subject string
	Payload []byte
	Headers map[string]string
}

// Store gives the relay access to pending records.
type Store interface {
	// Dispatch claims up to limit unpublished records (in creation order),
	// calls publish for each, and marks the successful ones as published,
	// atomically. It stops at the first publish error so ordering is kept,
	// leaving that record and the rest pending, and returns the number that
	// were published along with the error. Concurrent relays must not claim
	// the same records.
	Dispatch(ctx context.Context, limit int, publish func(context.Context, Record) error) (int, error)
}

// Options tunes the relay.
type Options struct {
	// BatchSize is the maximum number of records claimed per poll. Defaults to 100.
	BatchSize int
	// PollInterval is how long to wait when the outbox is empty. Defaults to 500ms.
	PollInterval time.Duration
	// ErrorBackoff is how long to wait after a failure. Defaults to 2s.
	ErrorBackoff time.Duration
}

func (o *Options) setDefaults() {
	if o.BatchSize == 0 {
		o.BatchSize = 100
	}
	if o.PollInterval == 0 {
		o.PollInterval = 500 * time.Millisecond
	}
	if o.ErrorBackoff == 0 {
		o.ErrorBackoff = 2 * time.Second
	}
}

// Relay publishes outbox records to the broker.
type Relay struct {
	store Store
	pub   messaging.Publisher
	log   *slog.Logger
	opts  Options
}

// NewRelay returns a Relay.
func NewRelay(store Store, pub messaging.Publisher, log *slog.Logger, opts Options) *Relay {
	opts.setDefaults()
	return &Relay{store: store, pub: pub, log: log, opts: opts}
}

// Run polls the store until ctx is cancelled. It is meant to be run as a
// runner.Task and never returns an error other than the context's.
func (r *Relay) Run(ctx context.Context) error {
	for {
		n, err := r.store.Dispatch(ctx, r.opts.BatchSize, r.publish)

		wait := r.opts.PollInterval
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.log.Error("outbox dispatch failed", "error", err, "published", n)
			wait = r.opts.ErrorBackoff
		case n == r.opts.BatchSize:
			wait = 0 // more work is probably waiting
		}

		if wait == 0 {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

func (r *Relay) publish(ctx context.Context, rec Record) error {
	err := r.pub.Publish(ctx, messaging.Message{
		ID:      rec.ID,
		Subject: rec.Subject,
		Data:    rec.Payload,
		Headers: rec.Headers,
	})
	if err != nil {
		return fmt.Errorf("outbox record %s: %w", rec.ID, err)
	}
	return nil
}
