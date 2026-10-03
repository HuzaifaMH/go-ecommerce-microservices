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
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/observability"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/outbox"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/outbox/pgstore"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/platform"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/runner"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/subjects"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/version"
	grpcadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/adapters/grpc"
	inventoryadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/adapters/inventory"
	metricsadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/adapters/metrics"
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

	inst, err := observability.Start(ctx, config.ServiceName)
	if err != nil {
		return err
	}
	defer func() {
		if err := inst.Close(5 * time.Second); err != nil {
			log.Warn("flushing traces failed", "error", err)
		}
	}()

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
	// order service starts even if inventory is not up yet. Calls are traced
	// (the trace continues into inventory) and measured.
	invConn, err := grpc.NewClient(cfg.InventoryAddr,
		append(inst.DialOptions(), grpc.WithTransportCredentials(insecure.NewCredentials()))...)
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
	}).WithMetrics(metricsadapter.New(inst.Registry))

	grpcSrv := grpc.NewServer(inst.ServerOptions()...)
	orderv1.RegisterOrderServiceServer(grpcSrv, grpcadapter.NewServer(svc, log))
	reflection.Register(grpcSrv) // lets grpcurl discover the API in development

	checks := health.New(2 * time.Second)
	checks.AddReadiness("postgres", pool.Ping)
	checks.AddReadiness("nats", platform.NATSReady(nc))

	serveGRPC, err := platform.ServeGRPC(ctx, cfg.GRPCAddr, grpcSrv, cfg.ShutdownTimeout)
	if err != nil {
		return err
	}
	// /metrics shares the internal health port; it is never exposed publicly.
	serveHTTP, err := platform.ServeHTTP(ctx, cfg.HTTPAddr, observability.WithMetrics(checks.Routes(), inst.Registry), cfg.ShutdownTimeout)
	if err != nil {
		return err
	}

	relay := outbox.NewRelay(store, messaging.NewJetStreamPublisher(js, messaging.WithObserver(inst.Messaging)), log, outbox.Options{})
	gauges := observability.NewOutboxGauges(inst.Registry)
	handlers := natsadapter.NewHandlers(svc)

	log.Info("listening", "grpc", cfg.GRPCAddr, "http", cfg.HTTPAddr, "inventory", cfg.InventoryAddr,
		"saga_timeout", cfg.SagaTimeout.String(), "tracing", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "")
	return runner.Run(ctx, log,
		serveGRPC,
		serveHTTP,
		relay.Run,
		func(ctx context.Context) error { return gauges.Run(ctx, store, 5*time.Second, log) },
		func(ctx context.Context) error {
			return natsadapter.Consume(ctx, js, store, handlers, log, inst.Messaging)
		},
		func(ctx context.Context) error {
			return svc.RunSweeper(ctx, cfg.SagaTimeout, cfg.SweepInterval, sweepBatch)
		},
	)
}
