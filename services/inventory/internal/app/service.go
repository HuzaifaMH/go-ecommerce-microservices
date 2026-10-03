// Package app implements the inventory use cases.
// It depends on interfaces (ports) and never on concrete adapters.
package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/observability"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/domain"
)

// Repository is the persistence port. Implementations take part in the
// transaction carried by ctx (see Transactor).
type Repository interface {
	GetItem(ctx context.Context, sku string) (domain.Item, error)
	// ListItems returns up to limit items with SKU greater than afterSKU, ordered by SKU.
	ListItems(ctx context.Context, limit int, afterSKU string) ([]domain.Item, error)
	// LockItems returns the known items among skus, locked until the transaction ends.
	// Unknown SKUs are simply absent from the result.
	LockItems(ctx context.Context, skus []string) (map[string]domain.Item, error)
	SaveItems(ctx context.Context, items map[string]domain.Item) error
	// FindReservation returns domain.ErrReservationNotFound when there is none.
	FindReservation(ctx context.Context, orderID string) (domain.Reservation, error)
	CreateReservation(ctx context.Context, r domain.Reservation) error
	SetReservationStatus(ctx context.Context, orderID string, status domain.ReservationStatus) error
}

// Transactor runs fn atomically. Repository calls and Events made with the
// context given to fn commit or roll back together.
type Transactor interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Events publishes the replies the saga expects. Implementations must make
// them part of the surrounding transaction (transactional outbox).
type Events interface {
	StockReserved(ctx context.Context, orderID string) error
	StockRejected(ctx context.Context, orderID, reason string, unavailableSKUs []string) error
	StockReleased(ctx context.Context, orderID string) error
}

// Outcomes reported to Metrics.
const (
	OutcomeReserved  = "reserved"  // stock was reserved
	OutcomeRejected  = "rejected"  // not enough stock; nothing reserved
	OutcomeDuplicate = "duplicate" // the order already had a reservation; the original outcome was re-sent
	OutcomeReleased  = "released"  // a reservation was released
	OutcomeNoop      = "noop"      // nothing to release (never reserved, or already released)
)

// Metrics records business outcomes for monitoring. Implementations must be
// cheap and must not fail.
type Metrics interface {
	// Reservation is called once per Reserve command that committed, with one of
	// OutcomeReserved, OutcomeRejected or OutcomeDuplicate.
	Reservation(outcome string)
	// Release is called once per Release command that committed, with
	// OutcomeReleased or OutcomeNoop.
	Release(outcome string)
}

type noMetrics struct{}

func (noMetrics) Reservation(string) {}
func (noMetrics) Release(string)     {}

// Service holds the inventory use cases.
type Service struct {
	repo    Repository
	tx      Transactor
	events  Events
	metrics Metrics
}

// NewService wires the use cases to their ports.
func NewService(repo Repository, tx Transactor, events Events) *Service {
	return &Service{repo: repo, tx: tx, events: events, metrics: noMetrics{}}
}

// WithMetrics sets where business metrics go. By default they are discarded.
// Outcomes are recorded only after the transaction has committed, so a step
// that is rolled back and retried is not counted twice.
func (s *Service) WithMetrics(m Metrics) *Service {
	if m == nil {
		m = noMetrics{}
	}
	s.metrics = m
	return s
}

// Reserve reserves stock for an order, all lines or none, and publishes
// StockReserved or StockRejected. It is idempotent per order: repeating the
// command for an order that already has a reservation re-publishes the
// original outcome and changes no stock.
//
// Insufficient stock is a business outcome, not an error: it is reported with
// StockRejected and Reserve returns nil. Errors wrapping
// domain.ErrInvalidReservation mean the command itself is malformed and
// retrying cannot help.
func (s *Service) Reserve(ctx context.Context, orderID string, lines []domain.Line) error {
	observability.SetSpanAttrs(ctx, "order.id", orderID)
	res, err := domain.NewReservation(orderID, lines)
	if err != nil {
		return err
	}

	var outcome string
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		existing, err := s.repo.FindReservation(ctx, orderID)
		switch {
		case err == nil:
			outcome = OutcomeDuplicate
			if existing.Status == domain.ReservationReleased {
				return s.events.StockRejected(ctx, orderID, "reservation was already released", nil)
			}
			return s.events.StockReserved(ctx, orderID)
		case !errors.Is(err, domain.ErrReservationNotFound):
			return fmt.Errorf("find reservation: %w", err)
		}

		items, err := s.repo.LockItems(ctx, res.SKUs())
		if err != nil {
			return fmt.Errorf("lock items: %w", err)
		}

		updated, err := res.Reserve(items)
		var short *domain.InsufficientStockError
		if errors.As(err, &short) {
			outcome = OutcomeRejected
			return s.events.StockRejected(ctx, orderID, "insufficient stock", short.SKUs)
		}
		if err != nil {
			return err
		}

		if err := s.repo.SaveItems(ctx, updated); err != nil {
			return fmt.Errorf("save items: %w", err)
		}
		if err := s.repo.CreateReservation(ctx, res); err != nil {
			return fmt.Errorf("create reservation: %w", err)
		}
		outcome = OutcomeReserved
		return s.events.StockReserved(ctx, orderID)
	})
	if err == nil {
		s.metrics.Reservation(outcome)
	}
	return err
}

// Release gives back the stock held for an order (saga compensation) and
// publishes StockReleased. It is idempotent: releasing an order with no
// reservation, or one that is already released, only re-publishes the event.
func (s *Service) Release(ctx context.Context, orderID string) error {
	if orderID == "" {
		return fmt.Errorf("%w: order ID is required", domain.ErrInvalidReservation)
	}
	observability.SetSpanAttrs(ctx, "order.id", orderID)

	var outcome string
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		outcome = OutcomeNoop
		res, err := s.repo.FindReservation(ctx, orderID)
		if errors.Is(err, domain.ErrReservationNotFound) {
			return s.events.StockReleased(ctx, orderID)
		}
		if err != nil {
			return fmt.Errorf("find reservation: %w", err)
		}
		if res.Status == domain.ReservationReleased {
			return s.events.StockReleased(ctx, orderID)
		}

		items, err := s.repo.LockItems(ctx, res.SKUs())
		if err != nil {
			return fmt.Errorf("lock items: %w", err)
		}
		updated, err := res.Release(items)
		if err != nil {
			return fmt.Errorf("release reservation %s: %w", orderID, err)
		}
		if err := s.repo.SaveItems(ctx, updated); err != nil {
			return fmt.Errorf("save items: %w", err)
		}
		if err := s.repo.SetReservationStatus(ctx, orderID, domain.ReservationReleased); err != nil {
			return fmt.Errorf("mark released: %w", err)
		}
		outcome = OutcomeReleased
		return s.events.StockReleased(ctx, orderID)
	})
	if err == nil {
		s.metrics.Release(outcome)
	}
	return err
}

// GetItem returns one item by SKU.
func (s *Service) GetItem(ctx context.Context, sku string) (domain.Item, error) {
	return s.repo.GetItem(ctx, sku)
}

// ListItems returns a page of items ordered by SKU and the SKU to pass as
// afterSKU for the next page ("" when there are no more).
func (s *Service) ListItems(ctx context.Context, pageSize int, afterSKU string) ([]domain.Item, string, error) {
	items, err := s.repo.ListItems(ctx, pageSize+1, afterSKU) // one extra row tells us whether more exist
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > pageSize {
		items = items[:pageSize]
		next = items[len(items)-1].SKU
	}
	return items, next, nil
}
