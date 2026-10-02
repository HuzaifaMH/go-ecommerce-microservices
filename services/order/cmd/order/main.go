// Command order is the entrypoint for the order-service.
// It only wires dependencies together; behaviour lives in internal/.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"

	orderv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/order/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/health"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/logging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/outbox"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/outbox/pgstore"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/platform"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/runner"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/subjects"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/version"
	grpcadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/adapters/grpc"
	inventoryadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/adapters/inventory"
	natsadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/adapters/nats"
	pgadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/adapters/postgres"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/config"
)

// sweepBatch is the maximum number of timed-out orders cancelled per sweep.
const sweepBatch = 100

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "order-service:", err)
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

	pool, err := platform.OpenPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pgadapter.Migrate(ctx, pool); err != nil {
		return err
	}

	nc, js, err := messaging.Connect(cfg.NATSURL, config.ServiceName)
	if err != nil {
		return err
	}
	defer nc.Close()
	// The saga publishes commands to the inventory and payment streams and
	// events to its own, so all three must exist before the first order.
	for _, stream := range []messaging.StreamConfig{subjects.OrderStream, subjects.InventoryStream, subjects.PaymentStream} {
		if _, err := messaging.EnsureStream(ctx, js, stream); err != nil {
			return err
		}
	}

	// gRPC client for pricing. The connection is established lazily, so the
	// order service starts even if inventory is not up yet.
	invConn, err := grpc.NewClient(cfg.InventoryAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("inventory client: %w", err)
	}
	defer func() { _ = invConn.Close() }()

	// Wiring: adapters implement the ports defined by the application layer.
	store := pgstore.New(pool)
	repo := pgadapter.NewRepository(store)
	publisher := natsadapter.NewPublisher(store)
	svc := app.NewService(app.Deps{
		Catalog:  inventoryadapter.NewClient(invConn, cfg.InventoryTimeout),
		Repo:     repo,
		Tx:       repo,
		Commands: publisher,
		Events:   publisher,
		Log:      log,
		NewID:    uuid.NewString,
		Now:      func() time.Time { return time.Now().UTC() },
	})

	grpcSrv := grpc.NewServer()
	orderv1.RegisterOrderServiceServer(grpcSrv, grpcadapter.NewServer(svc, log))
	reflection.Register(grpcSrv) // lets grpcurl discover the API in development

	checks := health.New(2 * time.Second)
	checks.AddReadiness("postgres", pool.Ping)
	checks.AddReadiness("nats", platform.NATSReady(nc))

	serveGRPC, err := platform.ServeGRPC(ctx, cfg.GRPCAddr, grpcSrv, cfg.ShutdownTimeout)
	if err != nil {
		return err
	}
	serveHTTP, err := platform.ServeHTTP(ctx, cfg.HTTPAddr, checks.Routes(), cfg.ShutdownTimeout)
	if err != nil {
		return err
	}

	relay := outbox.NewRelay(store, messaging.NewJetStreamPublisher(js), log, outbox.Options{})
	handlers := natsadapter.NewHandlers(svc)

	log.Info("listening", "grpc", cfg.GRPCAddr, "http", cfg.HTTPAddr, "inventory", cfg.InventoryAddr,
		"saga_timeout", cfg.SagaTimeout.String())
	return runner.Run(ctx, log,
		serveGRPC,
		serveHTTP,
		relay.Run,
		func(ctx context.Context) error { return natsadapter.Consume(ctx, js, store, handlers, log) },
		func(ctx context.Context) error {
			return svc.RunSweeper(ctx, cfg.SagaTimeout, cfg.SweepInterval, sweepBatch)
		},
	)
}
