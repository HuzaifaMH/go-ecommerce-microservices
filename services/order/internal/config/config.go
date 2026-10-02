// Package config loads and validates order-service configuration from the environment.
package config

import (
	"errors"
	"time"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/config"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/logging"
)

// ServiceName identifies this service in logs and the NATS connection.
const ServiceName = "order-service"

// Config is the complete runtime configuration.
type Config struct {
	Log             logging.Config
	GRPCAddr        string
	HTTPAddr        string // health endpoints
	DatabaseURL     string
	NATSURL         string
	ShutdownTimeout time.Duration

	// InventoryAddr is the inventory-service gRPC address, used for pricing.
	InventoryAddr string
	// InventoryTimeout bounds each price lookup.
	InventoryTimeout time.Duration

	// SagaTimeout is how long an order may wait for a reply before it is cancelled.
	SagaTimeout time.Duration
	// SweepInterval is how often the timeout sweeper runs.
	SweepInterval time.Duration
}

// Load reads the configuration from the process environment.
func Load() (Config, error) {
	return LoadFrom(config.FromEnv())
}

// LoadFrom reads the configuration from l. It returns every problem at once.
func LoadFrom(l *config.Loader) (Config, error) {
	cfg := Config{
		Log:              logging.ConfigFromEnv(l, ServiceName),
		GRPCAddr:         l.String("GRPC_ADDR", ":9090"),
		HTTPAddr:         l.String("HTTP_ADDR", ":8080"),
		DatabaseURL:      l.RequiredString("DATABASE_URL"),
		NATSURL:          l.String("NATS_URL", "nats://localhost:4222"),
		ShutdownTimeout:  l.Duration("SHUTDOWN_TIMEOUT", 15*time.Second),
		InventoryAddr:    l.String("INVENTORY_GRPC_ADDR", "localhost:9090"),
		InventoryTimeout: l.Duration("INVENTORY_TIMEOUT", 3*time.Second),
		SagaTimeout:      l.Duration("SAGA_TIMEOUT", 2*time.Minute),
		SweepInterval:    l.Duration("SWEEP_INTERVAL", 10*time.Second),
	}
	if err := l.Err(); err != nil {
		return Config{}, err
	}

	// Values that parse but make no sense are rejected too.
	var errs []error
	for name, d := range map[string]time.Duration{
		"INVENTORY_TIMEOUT": cfg.InventoryTimeout,
		"SAGA_TIMEOUT":      cfg.SagaTimeout,
		"SWEEP_INTERVAL":    cfg.SweepInterval,
	} {
		if d <= 0 {
			errs = append(errs, errors.New(name+" must be positive"))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
