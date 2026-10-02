// Package postgres is the PostgreSQL persistence adapter of the notification service.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/platform"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/adapters/postgres/sqlcgen"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/domain"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/migrations"
)

var _ app.Repository = (*Repository)(nil)

// Migrate applies the service's pending schema migrations.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return platform.Migrate(ctx, pool, migrations.FS)
}

// Repository implements app.Repository on PostgreSQL. Each operation is a
// single statement, so no surrounding transaction is needed.
type Repository struct {
	q *sqlcgen.Queries
}

// NewRepository returns a Repository.
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{q: sqlcgen.New(pool)}
}

// Ensure inserts each notification unless one for the same (order, kind,
// channel) exists, and returns the stored row for every input.
func (r *Repository) Ensure(ctx context.Context, ns []domain.Notification) ([]domain.Notification, error) {
	out := make([]domain.Notification, len(ns))
	for i, n := range ns {
		row, err := r.q.InsertNotification(ctx, sqlcgen.InsertNotificationParams{
			ID: n.ID, OrderID: n.OrderID, CustomerID: n.CustomerID,
			Kind: string(n.Kind), Channel: string(n.Channel),
			Recipient: n.Recipient, Subject: n.Subject, Body: n.Body,
			Status: string(n.Status), CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt,
		})
		if errors.Is(err, pgx.ErrNoRows) { // already recorded: read the existing one
			row, err = r.q.GetNotificationByKey(ctx, sqlcgen.GetNotificationByKeyParams{
				OrderID: n.OrderID, Kind: string(n.Kind), Channel: string(n.Channel),
			})
		}
		if err != nil {
			return nil, fmt.Errorf("record notification %s/%s/%s: %w", n.OrderID, n.Kind, n.Channel, err)
		}
		out[i] = toNotification(row)
	}
	return out, nil
}

func (r *Repository) MarkSent(ctx context.Context, id string, at time.Time) error {
	if err := r.q.MarkNotificationSent(ctx, sqlcgen.MarkNotificationSentParams{ID: id, UpdatedAt: at}); err != nil {
		return fmt.Errorf("mark sent: %w", err)
	}
	return nil
}

func (r *Repository) MarkFailed(ctx context.Context, id, reason string, at time.Time) error {
	err := r.q.MarkNotificationFailed(ctx, sqlcgen.MarkNotificationFailedParams{ID: id, FailureReason: reason, UpdatedAt: at})
	if err != nil {
		return fmt.Errorf("mark failed: %w", err)
	}
	return nil
}

func (r *Repository) List(ctx context.Context, q app.ListQuery) ([]domain.Notification, error) {
	if q.Limit < 0 || q.Limit > math.MaxInt32 {
		return nil, fmt.Errorf("list notifications: limit %d out of range", q.Limit)
	}
	limit := int32(q.Limit) //nolint:gosec // range checked above

	var (
		rows []sqlcgen.Notification
		err  error
	)
	if q.After == nil {
		rows, err = r.q.ListNotificationsFirstPage(ctx, sqlcgen.ListNotificationsFirstPageParams{
			OrderID: q.OrderID, CustomerID: q.CustomerID, PageLimit: limit,
		})
	} else {
		rows, err = r.q.ListNotificationsAfter(ctx, sqlcgen.ListNotificationsAfterParams{
			OrderID: q.OrderID, CustomerID: q.CustomerID,
			AfterCreatedAt: q.After.CreatedAt, AfterID: q.After.ID, PageLimit: limit,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}

	out := make([]domain.Notification, len(rows))
	for i, row := range rows {
		out[i] = toNotification(row)
	}
	return out, nil
}

func toNotification(row sqlcgen.Notification) domain.Notification {
	return domain.Notification{
		ID: row.ID, OrderID: row.OrderID, CustomerID: row.CustomerID,
		Kind: domain.Kind(row.Kind), Channel: domain.Channel(row.Channel),
		Recipient: row.Recipient, Subject: row.Subject, Body: row.Body,
		Status: domain.Status(row.Status), FailureReason: row.FailureReason,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}
