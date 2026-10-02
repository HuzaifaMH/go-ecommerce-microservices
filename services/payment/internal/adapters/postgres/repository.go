package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/outbox/pgstore"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/platform"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/internal/adapters/postgres/sqlcgen"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/internal/domain"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/payment/migrations"
)

var (
	_ app.Repository = (*Repository)(nil)
	_ app.Transactor = (*Repository)(nil)
)

// Migrate applies the service's pending schema migrations.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return platform.Migrate(ctx, pool, migrations.FS)
}

// Repository implements app.Repository and app.Transactor on PostgreSQL.
// Every call uses the transaction carried by ctx when there is one.
type Repository struct {
	store *pgstore.Store
}

// NewRepository returns a Repository on top of the shared outbox store.
func NewRepository(store *pgstore.Store) *Repository {
	return &Repository{store: store}
}

func (r *Repository) queries(ctx context.Context) *sqlcgen.Queries {
	return sqlcgen.New(r.store.Querier(ctx))
}

// InTx implements app.Transactor.
func (r *Repository) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return r.store.WithTx(ctx, fn)
}

func (r *Repository) FindByOrder(ctx context.Context, orderID string) (domain.Payment, error) {
	row, err := r.queries(ctx).GetPaymentByOrder(ctx, orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Payment{}, domain.ErrPaymentNotFound
	}
	if err != nil {
		return domain.Payment{}, fmt.Errorf("get payment: %w", err)
	}
	return toPayment(row), nil
}

// Create inserts the payment, or returns the order's existing one when
// another attempt got there first (UNIQUE order_id + ON CONFLICT DO NOTHING).
func (r *Repository) Create(ctx context.Context, p domain.Payment) (domain.Payment, error) {
	row, err := r.queries(ctx).InsertPayment(ctx, sqlcgen.InsertPaymentParams{
		ID:            p.ID,
		OrderID:       p.OrderID,
		CustomerID:    p.CustomerID,
		CurrencyCode:  p.Amount.CurrencyCode,
		AmountMinor:   p.Amount.AmountMinor,
		Status:        string(p.Status),
		FailureReason: p.FailureReason,
		ProviderRef:   p.ProviderRef,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return r.FindByOrder(ctx, p.OrderID)
	}
	if err != nil {
		return domain.Payment{}, fmt.Errorf("insert payment: %w", err)
	}
	return toPayment(row), nil
}

func toPayment(row sqlcgen.Payment) domain.Payment {
	return domain.Payment{
		ID:            row.ID,
		OrderID:       row.OrderID,
		CustomerID:    row.CustomerID,
		Amount:        domain.Money{CurrencyCode: row.CurrencyCode, AmountMinor: row.AmountMinor},
		Status:        domain.Status(row.Status),
		FailureReason: row.FailureReason,
		ProviderRef:   row.ProviderRef,
		CreatedAt:     row.CreatedAt,
	}
}
