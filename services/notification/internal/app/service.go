// Package app implements the notification use cases.
// It depends on interfaces (ports) and never on concrete adapters.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/observability"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/domain"
)

// Cursor identifies a position in the newest-first notification list.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// ListQuery selects a page of notifications, newest first.
type ListQuery struct {
	OrderID    string // empty: any order
	CustomerID string // empty: any customer
	Limit      int
	After      *Cursor // nil: first page
}

// Repository is the persistence port.
type Repository interface {
	// Ensure stores each notification that does not exist yet, where
	// "exists" means the same (order, kind, channel), and returns the stored
	// version of every one of them, new or existing, in the order given.
	Ensure(ctx context.Context, ns []domain.Notification) ([]domain.Notification, error)
	// MarkSent records a successful delivery of a pending notification.
	MarkSent(ctx context.Context, id string, at time.Time) error
	// MarkFailed records that a pending notification can never be delivered.
	MarkFailed(ctx context.Context, id, reason string, at time.Time) error
	List(ctx context.Context, q ListQuery) ([]domain.Notification, error)
}

// Sender delivers a notification (an email gateway or an SMS provider, in a
// real system). A notification that can never be delivered is reported as
// *domain.UndeliverableError; any other error is transient and the delivery
// will be retried.
type Sender interface {
	Send(ctx context.Context, n domain.Notification) error
}

// Outcomes reported to Metrics.Delivery.
const (
	OutcomeSent   = "sent"   // delivered
	OutcomeFailed = "failed" // permanently undeliverable; recorded and not retried
	OutcomeRetry  = "retry"  // a transient failure; the event will be redelivered
)

// Metrics records business outcomes for monitoring. Implementations must be
// cheap and must not fail.
type Metrics interface {
	// Delivery is called after every attempt to deliver a notification.
	Delivery(channel domain.Channel, outcome string)
}

type noMetrics struct{}

func (noMetrics) Delivery(domain.Channel, string) {}

// Service holds the notification use cases.
type Service struct {
	repo    Repository
	sender  Sender
	log     *slog.Logger
	newID   func() string
	now     func() time.Time
	metrics Metrics
}

// WithMetrics sets where business metrics go. By default they are discarded.
func (s *Service) WithMetrics(m Metrics) *Service {
	if m == nil {
		m = noMetrics{}
	}
	s.metrics = m
	return s
}

// NewService wires the use cases to their ports.
func NewService(repo Repository, sender Sender, log *slog.Logger, newID func() string, now func() time.Time) *Service {
	return &Service{repo: repo, sender: sender, log: log, newID: newID, now: now, metrics: noMetrics{}}
}

// NotifyOrderConfirmed tells the customer their order is confirmed.
func (s *Service) NotifyOrderConfirmed(ctx context.Context, orderID, customerID string, total domain.Money) error {
	ns, err := domain.OrderConfirmed(s.newID, orderID, customerID, total, s.now())
	if err != nil {
		return err
	}
	return s.deliver(ctx, ns)
}

// NotifyOrderCancelled tells the customer their order was cancelled and why.
func (s *Service) NotifyOrderCancelled(ctx context.Context, orderID, customerID, reason string) error {
	ns, err := domain.OrderCancelled(s.newID, orderID, customerID, reason, s.now())
	if err != nil {
		return err
	}
	return s.deliver(ctx, ns)
}

// deliver records the notifications and sends the ones still pending.
//
// Recording comes first and is keyed by (order, kind, channel), so a repeated
// event finds what was already recorded: notifications already sent are not
// sent again, and only pending ones are retried. A failure on one channel
// does not stop the others; the error is returned afterwards so the event is
// redelivered and the failed ones are tried again.
//
// Delivery is at-least-once: if the process dies after a message was sent
// but before it was marked sent, the redelivery sends it again.
func (s *Service) deliver(ctx context.Context, ns []domain.Notification) error {
	if len(ns) > 0 {
		observability.SetSpanAttrs(ctx, "order.id", ns[0].OrderID)
	}
	stored, err := s.repo.Ensure(ctx, ns)
	if err != nil {
		return fmt.Errorf("record notifications: %w", err)
	}

	var errs []error
	for _, n := range stored {
		if n.Status != domain.StatusPending {
			continue // already delivered, or permanently failed
		}

		sendErr := s.sender.Send(ctx, n)
		var undeliverable *domain.UndeliverableError
		switch {
		case errors.As(sendErr, &undeliverable):
			s.metrics.Delivery(n.Channel, OutcomeFailed)
			s.log.WarnContext(ctx, "notification undeliverable", "notification_id", n.ID, "order_id", n.OrderID,
				"channel", n.Channel, "reason", undeliverable.Reason)
			if err := s.repo.MarkFailed(ctx, n.ID, undeliverable.Reason, s.now()); err != nil {
				errs = append(errs, fmt.Errorf("mark %s failed: %w", n.ID, err))
			}
		case sendErr != nil:
			s.metrics.Delivery(n.Channel, OutcomeRetry)
			errs = append(errs, fmt.Errorf("send %s %s for order %s: %w", n.Kind, n.Channel, n.OrderID, sendErr))
		default:
			s.metrics.Delivery(n.Channel, OutcomeSent)
			if err := s.repo.MarkSent(ctx, n.ID, s.now()); err != nil {
				errs = append(errs, fmt.Errorf("mark %s sent: %w", n.ID, err))
			}
		}
	}
	return errors.Join(errs...)
}

// ListNotifications returns a page of notifications, newest first, and the
// cursor of the next page (nil when there are no more).
func (s *Service) ListNotifications(ctx context.Context, orderID, customerID string, pageSize int, after *Cursor) ([]domain.Notification, *Cursor, error) {
	items, err := s.repo.List(ctx, ListQuery{OrderID: orderID, CustomerID: customerID, Limit: pageSize + 1, After: after}) // one extra row tells us whether more exist
	if err != nil {
		return nil, nil, err
	}
	var next *Cursor
	if len(items) > pageSize {
		items = items[:pageSize]
		last := items[len(items)-1]
		next = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return items, next, nil
}
