// Package platform holds the start-up plumbing every service needs: opening
// PostgreSQL, applying embedded migrations, readiness checks, and binding
// listeners for the gRPC and health servers.
//
// Listeners are opened immediately (not when the task starts) so a port that
// is already in use fails the service at start-up, before any work begins.
package platform

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"
	"github.com/pressly/goose/v3"
	"google.golang.org/grpc"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/health"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/runner"
)

// OpenPostgres connects to PostgreSQL and verifies the connection.
func OpenPostgres(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}

// Migrate applies all pending goose migrations found at the root of fsys.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) error {
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }() // closes only the database/sql wrapper; the pool stays open

	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys)
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// NATSReady is a readiness check that fails while the connection is down.
func NATSReady(nc *nats.Conn) health.Check {
	return func(context.Context) error {
		if !nc.IsConnected() {
			return errors.New("not connected")
		}
		return nil
	}
}

// ServeGRPC binds addr now and returns a task serving srv with graceful shutdown.
func ServeGRPC(ctx context.Context, addr string, srv *grpc.Server, shutdownTimeout time.Duration) (runner.Task, error) {
	lis, err := new(net.ListenConfig).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen grpc on %s: %w", addr, err)
	}
	return runner.GRPCServer(srv, lis, shutdownTimeout), nil
}

// ServeHTTP binds addr now and returns a task serving h with graceful shutdown.
func ServeHTTP(ctx context.Context, addr string, h http.Handler, shutdownTimeout time.Duration) (runner.Task, error) {
	lis, err := new(net.ListenConfig).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen http on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	return runner.HTTPServer(srv, lis, shutdownTimeout), nil
}
