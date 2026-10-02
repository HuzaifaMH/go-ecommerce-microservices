// Command notification is the entrypoint for the notification-service.
// It only wires dependencies together; behaviour lives in internal/.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	notificationv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/notification/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/health"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/logging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/platform"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/runner"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/subjects"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/version"
	grpcadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/adapters/grpc"
	natsadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/adapters/nats"
	pgadapter "github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/adapters/postgres"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/adapters/sender/simulated"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/notification/internal/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "notification-service:", err)
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
	// This service only consumes the order stream, but it must exist before the consumer is created.
	if _, err := messaging.EnsureStream(ctx, js, subjects.OrderStream); err != nil {
		return err
	}

	// Wiring: adapters implement the ports defined by the application layer.
	svc := app.NewService(
		pgadapter.NewRepository(pool),
		simulated.New(log),
		log,
		uuid.NewString,
		func() time.Time { return time.Now().UTC() },
	)

	grpcSrv := grpc.NewServer()
	notificationv1.RegisterNotificationServiceServer(grpcSrv, grpcadapter.NewServer(svc, log))
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

	handlers := natsadapter.NewHandlers(svc)

	log.Info("listening", "grpc", cfg.GRPCAddr, "http", cfg.HTTPAddr)
	return runner.Run(ctx, log,
		serveGRPC,
		serveHTTP,
		func(ctx context.Context) error { return natsadapter.Consume(ctx, js, handlers, log) },
	)
}
