// Package app implements the payment use cases.
// It depends on interfaces (ports) and never on concrete adapters.
package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/observability"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/internal/domain"
)

// Provider charges a payment method (a card processor, in a real system).
//
// Charge must be idempotent per req.OrderID: the same order is never charged
// twice, and a repeated call returns the original result. Real providers
// support this with an idempotency key. A refusal is returned as
// *domain.DeclinedError; any other error means the outcome is unknown and
// the charge may be retried.
type Provider interface {
	Charge(ctx context.Context, req domain.ChargeRequest) (providerRef string, err error)
}

// Repository is the persistence port. Implementations take part in the
// transaction carried by ctx (see Transactor).
type Repository interface {
	// FindByOrder returns domain.ErrPaymentNotFound when the order has no payment.
	FindByOrder(ctx context.Context, orderID string) (domain.Payment, error)
	// Create stores p. If the order already has a payment, nothing is written
	// and the existing payment is returned, so concurrent attempts agree on one outcome.
	Create(ctx context.Context, p domain.Payment) (domain.Payment, error)
}

// Transactor runs fn atomically. Repository calls and Events made with the
// context given to fn commit or roll back together.
type Transactor interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Events publishes the replies the saga expects. Implementations must make
// them part of the surrounding transaction (transactional outbox).
type Events interface {
	PaymentSucceeded(ctx context.Context, orderID, paymentID string) error
	PaymentFailed(ctx context.Context, orderID, reason string) error
}

// Service holds the payment use cases.
type Service struct {
	provider Provider
	repo     Repository
	tx       Transactor
	events   Events
	newID    func() string
	metrics  Metrics
}

// Outcomes reported to Metrics.Charge.
const (
	OutcomeSucceeded = "succeeded" // the customer was charged
	OutcomeFailed    = "failed"    // the provider declined
	OutcomeDuplicate = "duplicate" // the order already had a payment; the stored outcome was re-sent
)

// Outcomes reported to Metrics.ProviderCall.
const (
	ProviderSucceeded = "succeeded"
	ProviderDeclined  = "declined"
	ProviderError     = "error" // an outage or timeout: the outcome is unknown and the charge is retried
)

// Metrics records business outcomes for monitoring. Implementations must be
// cheap and must not fail.
type Metrics interface {
	// Charge is called once per Charge command that committed.
	Charge(outcome string)
	// ProviderCall is called after every call to the payment provider, whatever
	// its result, with how long it took.
	ProviderCall(outcome string, took time.Duration)
}

type noMetrics struct{}

func (noMetrics) Charge(string)                      {}
func (noMetrics) ProviderCall(string, time.Duration) {}

// WithMetrics sets where business metrics go. By default they are discarded.
func (s *Service) WithMetrics(m Metrics) *Service {
	if m == nil {
		m = noMetrics{}
	}
	s.metrics = m
	return s
}

// NewService wires the use cases to their ports. newID generates payment IDs.
func NewService(provider Provider, repo Repository, tx Transactor, events Events, newID func() string) *Service {
	return &Service{provider: provider, repo: repo, tx: tx, events: events, newID: newID, metrics: noMetrics{}}
}

// Charge charges the customer for an order and publishes PaymentSucceeded or
// PaymentFailed.
//
// It is idempotent per order. The recorded payment is the single source of
// truth: repeating the command re-publishes the stored outcome without
// calling the provider again. A decline is a business outcome, recorded and
// reported with PaymentFailed, and Charge returns nil.
//
// The provider is called outside any database transaction, so a slow
// provider never holds a database connection. If the process dies after the
// provider charged but before the payment was stored, redelivery calls the
// provider again, and the provider's per-order idempotency returns the same
// result.
//
// Errors wrapping domain.ErrInvalidPayment mean the command is malformed or
// contradicts an earlier one for the same order; retrying cannot help.
func (s *Service) Charge(ctx context.Context, orderID, customerID string, amount domain.Money) error {
	observability.SetSpanAttrs(ctx, "order.id", orderID)
	req, err := domain.NewChargeRequest(orderID, customerID, amount)
	if err != nil {
		return err
	}

	existing, err := s.repo.FindByOrder(ctx, orderID)
	switch {
	case err == nil:
		if !existing.Matches(req) {
			return fmt.Errorf("%w: order %s already has a payment for a different amount or customer", domain.ErrInvalidPayment, orderID)
		}
		err := s.tx.InTx(ctx, func(ctx context.Context) error { return s.publish(ctx, existing) })
		if err == nil {
			s.metrics.Charge(OutcomeDuplicate)
		}
		return err
	case !errors.Is(err, domain.ErrPaymentNotFound):
		return fmt.Errorf("find payment: %w", err)
	}

	payment, err := s.callProvider(ctx, req)
	if err != nil {
		return err
	}

	var outcome string
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		stored, err := s.repo.Create(ctx, payment)
		if err != nil {
			return fmt.Errorf("store payment: %w", err)
		}
		switch {
		case stored.ID != payment.ID: // another attempt stored its payment first
			outcome = OutcomeDuplicate
		case stored.Status == domain.StatusSucceeded:
			outcome = OutcomeSucceeded
		default:
			outcome = OutcomeFailed
		}
		return s.publish(ctx, stored) // stored may be another attempt's payment: the first one wins
	})
	if err == nil {
		s.metrics.Charge(outcome)
	}
	return err
}

func (s *Service) callProvider(ctx context.Context, req domain.ChargeRequest) (domain.Payment, error) {
	start := time.Now()
	ref, err := s.provider.Charge(ctx, req)
	took := time.Since(start)

	var declined *domain.DeclinedError
	switch {
	case errors.As(err, &declined):
		s.metrics.ProviderCall(ProviderDeclined, took)
		return domain.NewFailed(s.newID(), req, declined.Reason), nil
	case err != nil:
		s.metrics.ProviderCall(ProviderError, took)
		return domain.Payment{}, fmt.Errorf("charge via provider: %w", err)
	}
	s.metrics.ProviderCall(ProviderSucceeded, took)
	return domain.NewSucceeded(s.newID(), req, ref), nil
}

func (s *Service) publish(ctx context.Context, p domain.Payment) error {
	if p.Status == domain.StatusSucceeded {
		return s.events.PaymentSucceeded(ctx, p.OrderID, p.ID)
	}
	return s.events.PaymentFailed(ctx, p.OrderID, p.FailureReason)
}

// GetPayment returns the payment recorded for an order.
func (s *Service) GetPayment(ctx context.Context, orderID string) (domain.Payment, error) {
	return s.repo.FindByOrder(ctx, orderID)
}
