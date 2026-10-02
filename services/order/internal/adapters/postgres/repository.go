package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/outbox/pgstore"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/platform"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/adapters/postgres/sqlcgen"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/domain"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/migrations"
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
// Every call uses the transaction carried by ctx when there is one, so it
// joins the transactions opened by InTx and by the inbox.
type Repository struct {
	store *pgstore.Store
}

// NewRepository returns a Repository on top of the shared outbox/inbox store.
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

// Create inserts the order and its items, or returns the customer's existing
// order for the same idempotency key (UNIQUE + ON CONFLICT DO NOTHING).
func (r *Repository) Create(ctx context.Context, o domain.Order) (domain.Order, bool, error) {
	q := r.queries(ctx)
	_, err := q.InsertOrder(ctx, sqlcgen.InsertOrderParams{
		ID: o.ID, CustomerID: o.CustomerID, IdempotencyKey: o.IdempotencyKey,
		CurrencyCode: o.Total.CurrencyCode, TotalMinor: o.Total.AmountMinor,
		Status: string(o.Status), CancelReason: o.CancelReason, RefundRequired: o.RefundRequired,
		CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		existing, err := r.FindByKey(ctx, o.CustomerID, o.IdempotencyKey)
		return existing, false, err
	}
	if err != nil {
		return domain.Order{}, false, fmt.Errorf("insert order: %w", err)
	}

	for _, it := range o.Items {
		qty, err := toInt32(it.Quantity)
		if err != nil {
			return domain.Order{}, false, fmt.Errorf("insert item %s: %w", it.SKU, err)
		}
		err = q.InsertOrderItem(ctx, sqlcgen.InsertOrderItemParams{
			OrderID: o.ID, Sku: it.SKU, Quantity: qty, UnitPriceMinor: it.UnitPrice.AmountMinor,
		})
		if err != nil {
			return domain.Order{}, false, fmt.Errorf("insert item %s: %w", it.SKU, err)
		}
	}
	return o, true, nil
}

func (r *Repository) FindByKey(ctx context.Context, customerID, key string) (domain.Order, error) {
	row, err := r.queries(ctx).GetOrderByKey(ctx, sqlcgen.GetOrderByKeyParams{CustomerID: customerID, IdempotencyKey: key})
	return r.one(ctx, row, err)
}

func (r *Repository) Get(ctx context.Context, id string) (domain.Order, error) {
	row, err := r.queries(ctx).GetOrder(ctx, id)
	return r.one(ctx, row, err)
}

func (r *Repository) GetForUpdate(ctx context.Context, id string) (domain.Order, error) {
	row, err := r.queries(ctx).GetOrderForUpdate(ctx, id)
	return r.one(ctx, row, err)
}

func (r *Repository) Save(ctx context.Context, o domain.Order) error {
	err := r.queries(ctx).UpdateOrder(ctx, sqlcgen.UpdateOrderParams{
		ID: o.ID, Status: string(o.Status), CancelReason: o.CancelReason,
		RefundRequired: o.RefundRequired, UpdatedAt: o.UpdatedAt,
	})
	if err != nil {
		return fmt.Errorf("update order: %w", err)
	}
	return nil
}

func (r *Repository) List(ctx context.Context, q app.ListQuery) ([]domain.Order, error) {
	limit, err := toInt32(q.Limit)
	if err != nil {
		return nil, fmt.Errorf("list orders: limit: %w", err)
	}

	var rows []sqlcgen.Order
	if q.After == nil {
		rows, err = r.queries(ctx).ListOrdersFirstPage(ctx, sqlcgen.ListOrdersFirstPageParams{CustomerID: q.CustomerID, PageLimit: limit})
	} else {
		rows, err = r.queries(ctx).ListOrdersAfter(ctx, sqlcgen.ListOrdersAfterParams{
			CustomerID: q.CustomerID, AfterCreatedAt: q.After.CreatedAt, AfterID: q.After.ID, PageLimit: limit,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	return r.many(ctx, rows)
}

func (r *Repository) LockStale(ctx context.Context, cutoff time.Time, limit int) ([]domain.Order, error) {
	n, err := toInt32(limit)
	if err != nil {
		return nil, fmt.Errorf("lock stale orders: limit: %w", err)
	}
	rows, err := r.queries(ctx).LockStaleOrders(ctx, sqlcgen.LockStaleOrdersParams{UpdatedAt: cutoff, Limit: n})
	if err != nil {
		return nil, fmt.Errorf("lock stale orders: %w", err)
	}
	return r.many(ctx, rows)
}

// one converts a single order row, mapping "no rows" to domain.ErrOrderNotFound.
func (r *Repository) one(ctx context.Context, row sqlcgen.Order, err error) (domain.Order, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Order{}, domain.ErrOrderNotFound
	}
	if err != nil {
		return domain.Order{}, fmt.Errorf("get order: %w", err)
	}
	orders, err := r.many(ctx, []sqlcgen.Order{row})
	if err != nil {
		return domain.Order{}, err
	}
	return orders[0], nil
}

// many converts order rows, loading all their items with one query.
func (r *Repository) many(ctx context.Context, rows []sqlcgen.Order) ([]domain.Order, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	itemRows, err := r.queries(ctx).GetItemsForOrders(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("get order items: %w", err)
	}

	items := make(map[string][]domain.Item, len(rows))
	currency := make(map[string]string, len(rows))
	for _, row := range rows {
		currency[row.ID] = row.CurrencyCode
	}
	for _, it := range itemRows {
		items[it.OrderID] = append(items[it.OrderID], domain.Item{
			SKU: it.Sku, Quantity: int(it.Quantity),
			UnitPrice: domain.Money{CurrencyCode: currency[it.OrderID], AmountMinor: it.UnitPriceMinor},
		})
	}

	out := make([]domain.Order, len(rows))
	for i, row := range rows {
		out[i] = domain.Order{
			ID: row.ID, CustomerID: row.CustomerID, IdempotencyKey: row.IdempotencyKey,
			Items:          items[row.ID],
			Total:          domain.Money{CurrencyCode: row.CurrencyCode, AmountMinor: row.TotalMinor},
			Status:         domain.Status(row.Status),
			CancelReason:   row.CancelReason,
			RefundRequired: row.RefundRequired,
			CreatedAt:      row.CreatedAt,
			UpdatedAt:      row.UpdatedAt,
		}
	}
	return out, nil
}

// toInt32 converts to the database's INTEGER type, failing instead of wrapping around.
func toInt32(n int) (int32, error) {
	if n < math.MinInt32 || n > math.MaxInt32 {
		return 0, fmt.Errorf("%d does not fit in 32 bits", n)
	}
	return int32(n), nil //nolint:gosec // range checked above
}
