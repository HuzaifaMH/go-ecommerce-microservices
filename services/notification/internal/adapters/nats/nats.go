// Package nats is the JetStream adapter of the notification service: it
// consumes order outcome events.
package nats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	orderv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/order/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/subjects"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/domain"
)

const consumerName = "notification"

// Notifier is the part of the application the event handlers call.
type Notifier interface {
	NotifyOrderConfirmed(ctx context.Context, orderID, customerID string, total domain.Money) error
	NotifyOrderCancelled(ctx context.Context, orderID, customerID, reason string) error
}

// Handlers decodes order events and calls the application.
type Handlers struct {
	notifier Notifier
}

// NewHandlers returns the event handlers.
func NewHandlers(n Notifier) *Handlers {
	return &Handlers{notifier: n}
}

// Handle routes an order event by subject. Events that can never succeed
// (garbage payloads, events without the data needed to notify) are permanent
// failures; delivery problems are returned as ordinary errors and retried.
func (h *Handlers) Handle(ctx context.Context, m messaging.Message) error {
	switch m.Subject {
	case subjects.OrderEvtConfirmed:
		var e orderv1.OrderConfirmed
		if err := proto.Unmarshal(m.Data, &e); err != nil {
			return undecodable(m.Subject, err)
		}
		total := domain.Money{CurrencyCode: e.GetTotal().GetCurrencyCode(), AmountMinor: e.GetTotal().GetAmountMinor()}
		return classify(h.notifier.NotifyOrderConfirmed(ctx, e.GetOrderId(), e.GetCustomerId(), total))

	case subjects.OrderEvtCancelled:
		var e orderv1.OrderCancelled
		if err := proto.Unmarshal(m.Data, &e); err != nil {
			return undecodable(m.Subject, err)
		}
		return classify(h.notifier.NotifyOrderCancelled(ctx, e.GetOrderId(), e.GetCustomerId(), e.GetReason()))

	default:
		return permanent(fmt.Errorf("unexpected subject %q", m.Subject))
	}
}

func undecodable(subject string, err error) error {
	return permanent(fmt.Errorf("decode %s: %w", subject, err))
}

func classify(err error) error {
	if errors.Is(err, domain.ErrInvalidEvent) {
		return permanent(err)
	}
	return err
}

func permanent(err error) error {
	return fmt.Errorf("%w: %w", messaging.ErrPermanent, err)
}

// Consume runs the order-event consumer until ctx is cancelled.
//
// It deliberately does not use the inbox: a notification is recorded under a
// unique (order, kind, channel) key before it is sent, so a redelivered event
// finds what was already done. The inbox transaction would also stay open
// while the sender is called.
func Consume(ctx context.Context, js jetstream.JetStream, h *Handlers, log *slog.Logger, obs messaging.Observer) error {
	return messaging.Consume(ctx, js, messaging.ConsumerConfig{
		Stream:         subjects.OrderStream.Name,
		Durable:        consumerName,
		FilterSubjects: []string{subjects.OrderEvtConfirmed, subjects.OrderEvtCancelled},
		Observer:       obs,
	}, log, h.Handle)
}
