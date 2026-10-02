// Package messaging wraps NATS JetStream with the conventions used by all
// services: every message has a unique ID (used for broker-side
// de-duplication and consumer-side idempotency), handlers acknowledge
// explicitly, and failures are retried unless marked permanent.
package messaging

import (
	"context"
	"errors"
)

// Header names carried on every message.
const (
	HeaderCorrelationID = "Correlation-Id"
	HeaderMessageType   = "Message-Type"
)

type correlationKey struct{}

// WithCorrelationID returns ctx carrying a correlation ID. Consumers set it
// from the incoming message so replies and logs can be traced back to the
// request that started the flow.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey{}, id)
}

// CorrelationID returns the correlation ID carried by ctx, or "".
func CorrelationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey{}).(string)
	return id
}

// ErrPermanent marks a handler error that retrying cannot fix (for example a
// malformed payload). The message is terminated instead of redelivered.
var ErrPermanent = errors.New("permanent failure")

// Message is a transport-agnostic message.
type Message struct {
	// ID uniquely identifies the message. Publishers set it; JetStream drops a
	// second publish with the same ID inside the stream's duplicate window.
	ID      string
	Subject string
	Data    []byte
	Headers map[string]string
}

// Handler processes one message. Returning nil acknowledges it, returning an
// error requests redelivery, and wrapping ErrPermanent terminates it.
type Handler func(ctx context.Context, m Message) error

// Publisher publishes messages to the broker.
type Publisher interface {
	Publish(ctx context.Context, m Message) error
}

// Inbox makes message handling idempotent. Once runs fn exactly once per
// (consumer, messageID): if the message was already processed it returns nil
// without calling fn. Implementations record the message ID in the same
// transaction as fn's side effects.
type Inbox interface {
	Once(ctx context.Context, consumer, messageID string, fn func(ctx context.Context) error) error
}

// Idempotent wraps h so each message ID is handled at most once per consumer.
func Idempotent(consumer string, inbox Inbox, h Handler) Handler {
	return func(ctx context.Context, m Message) error {
		return inbox.Once(ctx, consumer, m.ID, func(ctx context.Context) error {
			return h(ctx, m)
		})
	}
}
