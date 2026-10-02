// Package app implements the order use cases and drives the order saga.
// It depends on interfaces (ports) and never on concrete adapters.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/domain"
)

// ErrCatalogUnavailable is returned when prices cannot be fetched (the
// inventory service is down or slow). It is transient; the caller may retry.
var ErrCatalogUnavailable = errors.New("catalog unavailable")

// Catalog provides authoritative prices. Orders never trust prices sent by clients.
type Catalog interface {
	// Prices returns the unit price of every SKU, or *domain.UnknownSKUError.
	Prices(ctx context.Context, skus []string) (map[string]domain.Money, error)
}

// Cursor identifies a position in the newest-first order list.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// ListQuery selects a page of orders, newest first.
type ListQuery struct {
	CustomerID string // empty: all customers
	Limit      int
	After      *Cursor // nil: first page
}

// Repository is the persistence port. Implementations take part in the
// transaction carried by ctx (see Transactor).
type Repository interface {
	// Create stores o. If the customer already used the idempotency key, nothing
	// is written and the existing order is returned with created == false.
	Create(ctx context.Context, o domain.Order) (stored domain.Order, created bool, err error)
	// FindByKey returns domain.ErrOrderNotFound when the key is unused.
	FindByKey(ctx context.Context, customerID, idempotencyKey string) (domain.Order, error)
	// Get returns domain.ErrOrderNotFound for an unknown ID.
	Get(ctx context.Context, id string) (domain.Order, error)
	// GetForUpdate is Get, locking the order until the transaction ends.
	GetForUpdate(ctx context.Context, id string) (domain.Order, error)
	Save(ctx context.Context, o domain.Order) error
	List(ctx context.Context, q ListQuery) ([]domain.Order, error)
	// LockStale locks up to limit unfinished orders last updated before cutoff,
	// skipping rows another worker has locked.
	LockStale(ctx context.Context, cutoff time.Time, limit int) ([]domain.Order, error)
}

// Transactor runs fn atomically. Repository calls, Commands and Events made
// with the context given to fn commit or roll back together.
type Transactor interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Commands sends saga commands to the other services. Implementations must
// make them part of the surrounding transaction (transactional outbox).
type Commands interface {
	ReserveStock(ctx context.Context, orderID string, lines []domain.Line) error
	ReleaseStock(ctx context.Context, orderID string) error
	ChargePayment(ctx context.Context, orderID, customerID string, total domain.Money) error
}

// Events announces order outcomes. Implementations must make them part of the
// surrounding transaction (transactional outbox).
type Events interface {
	OrderConfirmed(ctx context.Context, o domain.Order) error
	OrderCancelled(ctx context.Context, o domain.Order) error
}

// Service holds the order use cases and the saga steps.
type Service struct {
	catalog  Catalog
	repo     Repository
	tx       Transactor
	commands Commands
	events   Events
	log      *slog.Logger
	newID    func() string
	now      func() time.Time
}

// Deps are the collaborators of a Service.
type Deps struct {
	Catalog  Catalog
	Repo     Repository
	Tx       Transactor
	Commands Commands
	Events   Events
	Log      *slog.Logger
	NewID    func() string
	Now      func() time.Time
}

// NewService wires the use cases to their ports.
func NewService(d Deps) *Service {
	return &Service{catalog: d.Catalog, repo: d.Repo, tx: d.Tx, commands: d.Commands, events: d.Events,
		log: d.Log, newID: d.NewID, now: d.Now}
}

// CreateOrder accepts an order and starts the saga by asking inventory to
// reserve stock. It returns the order in the pending state; the outcome
// arrives asynchronously.
//
// It is idempotent per (customer, idempotencyKey): repeating a request returns
// the original order and starts nothing new. Reusing a key for a different
// request fails with domain.ErrIdempotencyConflict.
func (s *Service) CreateOrder(ctx context.Context, customerID, idempotencyKey string, lines []domain.Line) (domain.Order, error) {
	if customerID == "" || idempotencyKey == "" {
		return domain.Order{}, fmt.Errorf("%w: customer ID and idempotency key are required", domain.ErrInvalidOrder)
	}
	lines, err := domain.NormalizeLines(lines)
	if err != nil {
		return domain.Order{}, err
	}

	// A repeat is answered without touching the catalog.
	existing, err := s.repo.FindByKey(ctx, customerID, idempotencyKey)
	switch {
	case err == nil:
		return sameOrConflict(existing, customerID, lines)
	case !errors.Is(err, domain.ErrOrderNotFound):
		return domain.Order{}, fmt.Errorf("find order by key: %w", err)
	}

	skus := make([]string, len(lines))
	for i, l := range lines {
		skus[i] = l.SKU
	}
	prices, err := s.catalog.Prices(ctx, skus) // network call: outside any transaction
	if err != nil {
		return domain.Order{}, fmt.Errorf("look up prices: %w", err)
	}
	order, err := domain.NewOrder(s.newID(), customerID, idempotencyKey, lines, prices, s.now())
	if err != nil {
		return domain.Order{}, err
	}

	var result domain.Order
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		stored, created, err := s.repo.Create(ctx, order)
		if err != nil {
			return fmt.Errorf("create order: %w", err)
		}
		if !created { // lost a race with an identical concurrent request
			result, err = sameOrConflict(stored, customerID, lines)
			return err
		}
		result = stored
		return s.commands.ReserveStock(ctx, stored.ID, stored.Lines())
	})
	return result, err
}

func sameOrConflict(o domain.Order, customerID string, lines []domain.Line) (domain.Order, error) {
	if !o.SameRequest(customerID, lines) {
		return domain.Order{}, domain.ErrIdempotencyConflict
	}
	return o, nil
}

// CancelOrder cancels an order that is pending or has its stock reserved,
// releases any stock held for it, and announces the cancellation. A confirmed
// order cannot be cancelled (domain.ErrCannotCancel). Cancelling an order that
// is already cancelled returns it unchanged.
func (s *Service) CancelOrder(ctx context.Context, id, reason string) (domain.Order, error) {
	if reason == "" {
		reason = "cancelled by customer"
	}
	var result domain.Order
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		o, err := s.repo.GetForUpdate(ctx, id)
		if err != nil {
			return err
		}
		if o.Status == domain.StatusCancelled {
			result = o
			return nil
		}
		result, err = s.cancel(ctx, o, reason)
		return err
	})
	return result, err
}

// cancel cancels o, releases its stock and announces it. The release is sent
// even if stock may not be reserved yet: commands are processed in order, so
// it either undoes the reservation or finds nothing to do.
func (s *Service) cancel(ctx context.Context, o domain.Order, reason string) (domain.Order, error) {
	cancelled, err := o.Cancel(reason, s.now())
	if err != nil {
		return o, err
	}
	if err := s.repo.Save(ctx, cancelled); err != nil {
		return o, fmt.Errorf("save order: %w", err)
	}
	if err := s.commands.ReleaseStock(ctx, cancelled.ID); err != nil {
		return o, err
	}
	if err := s.events.OrderCancelled(ctx, cancelled); err != nil {
		return o, err
	}
	return cancelled, nil
}

// react runs one saga step on a locked order inside a transaction.
func (s *Service) react(ctx context.Context, orderID string, step func(ctx context.Context, o domain.Order) error) error {
	return s.tx.InTx(ctx, func(ctx context.Context) error {
		o, err := s.repo.GetForUpdate(ctx, orderID)
		if err != nil {
			return err
		}
		return step(ctx, o)
	})
}

// HandleStockReserved is the saga's reaction to inventory reserving stock:
// charge the customer. A reservation that arrives for an already cancelled
// order is released again, otherwise that stock would stay locked forever.
// Duplicates are ignored.
func (s *Service) HandleStockReserved(ctx context.Context, orderID string) error {
	return s.react(ctx, orderID, func(ctx context.Context, o domain.Order) error {
		switch o.Status {
		case domain.StatusPending:
			next, err := o.StockReserved(s.now())
			if err != nil {
				return err
			}
			if err := s.repo.Save(ctx, next); err != nil {
				return fmt.Errorf("save order: %w", err)
			}
			return s.commands.ChargePayment(ctx, next.ID, next.CustomerID, next.Total)
		case domain.StatusCancelled:
			s.log.Warn("stock reserved for a cancelled order; releasing it", "order_id", o.ID)
			return s.commands.ReleaseStock(ctx, o.ID)
		default:
			return nil // duplicate
		}
	})
}

// HandleStockRejected cancels a pending order whose stock could not be
// reserved. Nothing was reserved, so there is nothing to release.
func (s *Service) HandleStockRejected(ctx context.Context, orderID, reason string) error {
	return s.react(ctx, orderID, func(ctx context.Context, o domain.Order) error {
		if o.Status != domain.StatusPending {
			return nil // duplicate, or the order was cancelled meanwhile
		}
		cancelled, err := o.Cancel("out of stock: "+reason, s.now())
		if err != nil {
			return err
		}
		if err := s.repo.Save(ctx, cancelled); err != nil {
			return fmt.Errorf("save order: %w", err)
		}
		return s.events.OrderCancelled(ctx, cancelled)
	})
}

// HandlePaymentSucceeded confirms an order whose stock is reserved. If the
// order was cancelled in the meantime the customer has been charged for
// nothing: the order is flagged as needing a refund and an error is logged.
func (s *Service) HandlePaymentSucceeded(ctx context.Context, orderID string) error {
	return s.react(ctx, orderID, func(ctx context.Context, o domain.Order) error {
		switch o.Status {
		case domain.StatusStockReserved:
			confirmed, err := o.Confirm(s.now())
			if err != nil {
				return err
			}
			if err := s.repo.Save(ctx, confirmed); err != nil {
				return fmt.Errorf("save order: %w", err)
			}
			return s.events.OrderConfirmed(ctx, confirmed)
		case domain.StatusCancelled:
			if o.RefundRequired {
				return nil // already flagged
			}
			s.log.Error("payment succeeded for a cancelled order; refund required", "order_id", o.ID)
			if err := s.repo.Save(ctx, o.MarkRefundRequired(s.now())); err != nil {
				return fmt.Errorf("save order: %w", err)
			}
			return nil
		case domain.StatusConfirmed:
			return nil // duplicate
		default:
			return fmt.Errorf("%w: payment succeeded while %s", domain.ErrInvalidTransition, o.Status)
		}
	})
}

// HandlePaymentFailed rolls back an order whose payment was declined: it is
// cancelled, its stock released, and the cancellation announced.
func (s *Service) HandlePaymentFailed(ctx context.Context, orderID, reason string) error {
	return s.react(ctx, orderID, func(ctx context.Context, o domain.Order) error {
		switch o.Status {
		case domain.StatusStockReserved:
			_, err := s.cancel(ctx, o, "payment failed: "+reason)
			return err
		case domain.StatusCancelled, domain.StatusConfirmed:
			return nil // duplicate or already resolved
		default:
			return fmt.Errorf("%w: payment failed while %s", domain.ErrInvalidTransition, o.Status)
		}
	})
}

// ExpireStale cancels unfinished orders that have waited longer than timeout
// for a reply, releasing their stock. It guards against a lost reply (for
// example a command that exhausted its redeliveries) leaving an order stuck.
// It returns the number of orders cancelled.
func (s *Service) ExpireStale(ctx context.Context, timeout time.Duration, limit int) (int, error) {
	var n int
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		stale, err := s.repo.LockStale(ctx, s.now().Add(-timeout), limit)
		if err != nil {
			return fmt.Errorf("find stale orders: %w", err)
		}
		for _, o := range stale {
			s.log.Warn("cancelling order that timed out", "order_id", o.ID, "status", o.Status)
			if _, err := s.cancel(ctx, o, "saga timeout"); err != nil {
				return err
			}
		}
		n = len(stale)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// RunSweeper calls ExpireStale every interval until ctx is cancelled. It is
// meant to be run as a runner.Task. A failed sweep is logged and retried on
// the next tick.
func (s *Service) RunSweeper(ctx context.Context, timeout, interval time.Duration, batch int) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			n, err := s.ExpireStale(ctx, timeout, batch)
			switch {
			case err != nil && ctx.Err() == nil:
				s.log.Error("sweeping stale orders failed", "error", err)
			case n > 0:
				s.log.Info("cancelled timed-out orders", "count", n)
			}
		}
	}
}

// GetOrder returns one order.
func (s *Service) GetOrder(ctx context.Context, id string) (domain.Order, error) {
	return s.repo.Get(ctx, id)
}

// ListOrders returns a page of orders, newest first, and the cursor of the next
// page (nil when there are no more).
func (s *Service) ListOrders(ctx context.Context, customerID string, pageSize int, after *Cursor) ([]domain.Order, *Cursor, error) {
	orders, err := s.repo.List(ctx, ListQuery{CustomerID: customerID, Limit: pageSize + 1, After: after}) // one extra row tells us whether more exist
	if err != nil {
		return nil, nil, err
	}
	var next *Cursor
	if len(orders) > pageSize {
		orders = orders[:pageSize]
		last := orders[len(orders)-1]
		next = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return orders, next, nil
}
