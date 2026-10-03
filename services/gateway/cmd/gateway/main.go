// Command gateway is the entrypoint for the api-gateway, the public REST/JSON
// front door of the system. It only wires dependencies together; behaviour
// lives in internal/.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"

	inventoryv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/inventory/v1"
	notificationv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/notification/v1"
	orderv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/order/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/health"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/logging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/observability"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/platform"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/runner"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/version"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/gateway/internal/api"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/gateway/internal/auth"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/gateway/internal/clients"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/gateway/internal/config"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/gateway/internal/ratelimit"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "api-gateway:", err)
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

	keys, devIssuer, err := keySource(cfg, log)
	if err != nil {
		return err
	}
	verifier, err := auth.NewVerifier(keys, auth.VerifierOptions{Issuer: cfg.JWTIssuer, Audience: cfg.JWTAudience})
	if err != nil {
		return err
	}

	// Backend connections. They connect lazily, so the gateway starts even when
	// a backend is not up yet; readiness reports the truth. Every call is traced
	// (the trace continues into the service) and measured.
	orderConn, err := clients.Dial(cfg.OrderAddr, cfg.BackendTimeout, inst.DialOptions()...)
	if err != nil {
		return err
	}
	defer func() { _ = orderConn.Close() }()
	inventoryConn, err := clients.Dial(cfg.InventoryAddr, cfg.BackendTimeout, inst.DialOptions()...)
	if err != nil {
		return err
	}
	defer func() { _ = inventoryConn.Close() }()
	notificationConn, err := clients.Dial(cfg.NotificationAddr, cfg.BackendTimeout, inst.DialOptions()...)
	if err != nil {
		return err
	}
	defer func() { _ = notificationConn.Close() }()

	checks := health.New(2 * time.Second)
	checks.AddReadiness("order-service", backendReady(orderConn))
	checks.AddReadiness("inventory-service", backendReady(inventoryConn))
	checks.AddReadiness("notification-service", backendReady(notificationConn))

	var limiter *ratelimit.Limiter
	if cfg.RateLimitRPS > 0 {
		limiter = ratelimit.New(cfg.RateLimitRPS, cfg.RateLimitBurst, ratelimit.Options{})
	}

	handler := api.NewHandler(api.Deps{
		Orders:        orderv1.NewOrderServiceClient(orderConn),
		Inventory:     inventoryv1.NewInventoryServiceClient(inventoryConn),
		Notifications: notificationv1.NewNotificationServiceClient(notificationConn),
		Verifier:      verifier,
		DevIssuer:     devIssuer,
		Limiter:       limiter,
		Health:        checks.Routes(),
		CORSOrigins:   cfg.CORSOrigins,
		MaxBodyBytes:  cfg.MaxBodyBytes,
		Metrics:       api.NewHTTPMetrics(inst.Registry),
		Log:           log,
	})

	lis, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen http on %s: %w", cfg.HTTPAddr, err)
	}
	// Timeouts matter on a public server: they stop slow or stalled clients from holding connections open.
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Metrics are served on their own internal port: the main one is public.
	serveMetrics, err := platform.ServeHTTP(ctx, cfg.MetricsAddr, observability.MetricsHandler(inst.Registry), cfg.ShutdownTimeout)
	if err != nil {
		return err
	}

	tasks := []runner.Task{runner.HTTPServer(srv, lis, cfg.ShutdownTimeout), serveMetrics}
	if limiter != nil {
		tasks = append(tasks, func(ctx context.Context) error { return limiter.Run(ctx, time.Minute) })
	}

	log.Info("listening", "http", cfg.HTTPAddr, "metrics", cfg.MetricsAddr, "order", cfg.OrderAddr, "inventory", cfg.InventoryAddr,
		"notification", cfg.NotificationAddr, "rate_limit_rps", cfg.RateLimitRPS,
		"tracing", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "")
	return runner.Run(ctx, log, tasks...)
}

// keySource chooses where signing keys come from: a generated development
// key, a PEM file, or a JWKS endpoint. The dev issuer is non-nil only in the first case.
func keySource(cfg config.Config, log *slog.Logger) (auth.KeySet, *auth.DevIssuer, error) {
	switch {
	case cfg.DevAuth:
		issuer, err := auth.NewDevIssuer(cfg.JWTIssuer, cfg.JWTAudience, nil)
		if err != nil {
			return nil, nil, err
		}
		log.Warn("DEV_AUTH is enabled: POST /dev/token mints tokens for anyone, including admins. Never run this way in production.")
		return issuer.KeySet(), issuer, nil

	case cfg.JWTPublicKeyFile != "":
		pemData, err := os.ReadFile(cfg.JWTPublicKeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read JWT_PUBLIC_KEY_FILE: %w", err)
		}
		key, err := auth.ParsePublicKeyPEM(pemData)
		if err != nil {
			return nil, nil, fmt.Errorf("JWT_PUBLIC_KEY_FILE: %w", err)
		}
		log.Info("verifying tokens with a public key from file", "file", cfg.JWTPublicKeyFile)
		return auth.NewStaticKey(key, ""), nil, nil

	default:
		log.Info("verifying tokens with keys from a JWKS endpoint", "url", cfg.JWKSURL)
		return auth.NewJWKS(cfg.JWKSURL, auth.JWKSOptions{}), nil, nil
	}
}

// backendReady reports a backend as not ready only while its connection is in
// a failed state. A connection that has not been used yet (idle) is fine; we
// nudge it to connect so a down backend shows up in readiness.
func backendReady(conn *grpc.ClientConn) health.Check {
	conn.Connect()
	return func(context.Context) error {
		switch s := conn.GetState(); s {
		case connectivity.TransientFailure, connectivity.Shutdown:
			return errors.New("connection is " + s.String())
		}
		return nil
	}
}
