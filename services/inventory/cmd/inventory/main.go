// Command inventory is the entrypoint for the inventory-service.
// It only wires dependencies together; behaviour lives in internal/.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	inventoryv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/inventory/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/health"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/logging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/outbox"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/outbox/pgstore"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/runner"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/subjects"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/version"
	grpcadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/adapters/grpc"
	natsadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/adapters/nats"
	pgadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/adapters/postgres"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/inventory/internal/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "inventory-service:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	log, err := logging.New(os.Stdout, cfg.Log)
	if err != nil {
		return err
	}
	log.Info("starting", "version", version.String())

	ctx := context.Background()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping postgres: %w", err)
	}
	if err := pgadapter.Migrate(ctx, pool); err != nil {
		return err
	}
	if cfg.SeedDemoData {
		if err := pgadapter.SeedDemoData(ctx, pool); err != nil {
			return err
		}
		log.Info("demo stock seeded")
	}

	nc, js, err := messaging.Connect(cfg.NATSURL, config.ServiceName)
	if err != nil {
		return err
	}
	defer nc.Close()
	if _, err := messaging.EnsureStream(ctx, js, subjects.InventoryStream); err != nil {
		return err
	}

	// Wiring: adapters implement the ports defined by the application layer.
	store := pgstore.New(pool)
	repo := pgadapter.NewRepository(store)
	svc := app.NewService(repo, repo, natsadapter.NewEvents(store))

	grpcSrv := grpc.NewServer()
	inventoryv1.RegisterInventoryServiceServer(grpcSrv, grpcadapter.NewServer(svc, log))
	reflection.Register(grpcSrv) // lets grpcurl discover the API in development

	checks := health.New(2 * time.Second)
	checks.AddReadiness("postgres", pool.Ping)
	checks.AddReadiness("nats", func(context.Context) error {
		if !nc.IsConnected() {
			return errors.New("not connected")
		}
		return nil
	})

	grpcLis, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listen grpc: %w", err)
	}
	httpLis, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen http: %w", err)
	}
	httpSrv := &http.Server{Handler: checks.Routes(), ReadHeaderTimeout: 5 * time.Second}

	relay := outbox.NewRelay(store, messaging.NewJetStreamPublisher(js), log, outbox.Options{})
	handlers := natsadapter.NewHandlers(svc)

	log.Info("listening", "grpc", cfg.GRPCAddr, "http", cfg.HTTPAddr)
	return runner.Run(ctx, log,
		runner.GRPCServer(grpcSrv, grpcLis, cfg.ShutdownTimeout),
		runner.HTTPServer(httpSrv, httpLis, cfg.ShutdownTimeout),
		relay.Run,
		func(ctx context.Context) error { return natsadapter.Consume(ctx, js, store, handlers, log) },
	)
}
