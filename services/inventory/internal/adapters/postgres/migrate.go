package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/adapters/postgres/sqlcgen"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/migrations"
)

// Migrate applies all pending schema migrations.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }() // closes only the database/sql wrapper; the pool stays open

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// demoItems is sample stock for local development.
var demoItems = []sqlcgen.UpsertItemParams{
	{Sku: "BOOK-GO-001", Name: "The Go Programming Language", CurrencyCode: "USD", UnitPriceMinor: 3999, OnHand: 100},
	{Sku: "BOOK-GRPC-001", Name: "gRPC: Up and Running", CurrencyCode: "USD", UnitPriceMinor: 3499, OnHand: 50},
	{Sku: "MUG-GOPHER-001", Name: "Gopher Mug", CurrencyCode: "USD", UnitPriceMinor: 1299, OnHand: 200},
	{Sku: "TSHIRT-GO-M", Name: "Go T-Shirt (M)", CurrencyCode: "USD", UnitPriceMinor: 2199, OnHand: 75},
	{Sku: "STICKER-NATS-001", Name: "NATS Sticker Pack", CurrencyCode: "USD", UnitPriceMinor: 499, OnHand: 5},
}

// SeedDemoData inserts sample stock if it is not there yet. It never changes existing rows.
func SeedDemoData(ctx context.Context, pool *pgxpool.Pool) error {
	q := sqlcgen.New(pool)
	for _, it := range demoItems {
		if err := q.UpsertItem(ctx, it); err != nil {
			return fmt.Errorf("seed %s: %w", it.Sku, err)
		}
	}
	return nil
}
