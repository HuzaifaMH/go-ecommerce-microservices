package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/outbox/pgstore"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/adapters/postgres/sqlcgen"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/domain"
)

var (
	_ app.Repository = (*Repository)(nil)
	_ app.Transactor = (*Repository)(nil)
)

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

func (r *Repository) GetItem(ctx context.Context, sku string) (domain.Item, error) {
	row, err := r.queries(ctx).GetItem(ctx, sku)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Item{}, domain.ErrItemNotFound
	}
	if err != nil {
		return domain.Item{}, fmt.Errorf("get item: %w", err)
	}
	return toItem(row), nil
}

func (r *Repository) ListItems(ctx context.Context, limit int, afterSKU string) ([]domain.Item, error) {
	n, err := toInt32(limit)
	if err != nil {
		return nil, fmt.Errorf("list items: limit: %w", err)
	}
	rows, err := r.queries(ctx).ListItems(ctx, sqlcgen.ListItemsParams{Sku: afterSKU, Limit: n})
	if err != nil {
		return nil, fmt.Errorf("list items: %w", err)
	}
	items := make([]domain.Item, len(rows))
	for i, row := range rows {
		items[i] = toItem(row)
	}
	return items, nil
}

func (r *Repository) LockItems(ctx context.Context, skus []string) (map[string]domain.Item, error) {
	rows, err := r.queries(ctx).LockItems(ctx, skus)
	if err != nil {
		return nil, fmt.Errorf("lock items: %w", err)
	}
	items := make(map[string]domain.Item, len(rows))
	for _, row := range rows {
		items[row.Sku] = toItem(row)
	}
	return items, nil
}

func (r *Repository) SaveItems(ctx context.Context, items map[string]domain.Item) error {
	skus := make([]string, 0, len(items))
	for sku := range items {
		skus = append(skus, sku)
	}
	sort.Strings(skus)

	q := r.queries(ctx)
	for _, sku := range skus {
		reserved, err := toInt32(items[sku].Reserved)
		if err != nil {
			return fmt.Errorf("set reserved for %s: %w", sku, err)
		}
		if err := q.SetReserved(ctx, sqlcgen.SetReservedParams{Sku: sku, Reserved: reserved}); err != nil {
			return fmt.Errorf("set reserved for %s: %w", sku, err)
		}
	}
	return nil
}

func (r *Repository) FindReservation(ctx context.Context, orderID string) (domain.Reservation, error) {
	q := r.queries(ctx)
	head, err := q.GetReservation(ctx, orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Reservation{}, domain.ErrReservationNotFound
	}
	if err != nil {
		return domain.Reservation{}, fmt.Errorf("get reservation: %w", err)
	}
	lines, err := q.GetReservationLines(ctx, orderID)
	if err != nil {
		return domain.Reservation{}, fmt.Errorf("get reservation lines: %w", err)
	}

	res := domain.Reservation{OrderID: head.OrderID, Status: domain.ReservationStatus(head.Status)}
	for _, l := range lines {
		res.Lines = append(res.Lines, domain.Line{SKU: l.Sku, Quantity: int(l.Quantity)})
	}
	return res, nil
}

func (r *Repository) CreateReservation(ctx context.Context, res domain.Reservation) error {
	q := r.queries(ctx)
	if err := q.CreateReservation(ctx, sqlcgen.CreateReservationParams{OrderID: res.OrderID, Status: string(res.Status)}); err != nil {
		return fmt.Errorf("insert reservation: %w", err)
	}
	for _, l := range res.Lines {
		qty, err := toInt32(l.Quantity)
		if err != nil {
			return fmt.Errorf("insert reservation line %s: %w", l.SKU, err)
		}
		if err := q.CreateReservationLine(ctx, sqlcgen.CreateReservationLineParams{OrderID: res.OrderID, Sku: l.SKU, Quantity: qty}); err != nil {
			return fmt.Errorf("insert reservation line %s: %w", l.SKU, err)
		}
	}
	return nil
}

func (r *Repository) SetReservationStatus(ctx context.Context, orderID string, status domain.ReservationStatus) error {
	err := r.queries(ctx).SetReservationStatus(ctx, sqlcgen.SetReservationStatusParams{OrderID: orderID, Status: string(status)})
	if err != nil {
		return fmt.Errorf("set reservation status: %w", err)
	}
	return nil
}

// toInt32 converts to the database's INTEGER type, failing instead of wrapping around.
func toInt32(n int) (int32, error) {
	if n < math.MinInt32 || n > math.MaxInt32 {
		return 0, fmt.Errorf("%d does not fit in 32 bits", n)
	}
	return int32(n), nil //nolint:gosec // range checked above
}

func toItem(row sqlcgen.StockItem) domain.Item {
	return domain.Item{
		SKU:       row.Sku,
		Name:      row.Name,
		UnitPrice: domain.Money{CurrencyCode: row.CurrencyCode, AmountMinor: row.UnitPriceMinor},
		OnHand:    int(row.OnHand),
		Reserved:  int(row.Reserved),
	}
}
