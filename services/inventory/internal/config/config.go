// Package config loads and validates inventory-service configuration from the environment.
package config

import (
	"time"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/config"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/logging"
)

// ServiceName identifies this service in logs and the NATS connection.
const ServiceName = "inventory-service"

// Config is the complete runtime configuration.
type Config struct {
	Log             logging.Config
	GRPCAddr        string
	HTTPAddr        string // health endpoints
	DatabaseURL     string
	NATSURL         string
	SeedDemoData    bool
	ShutdownTimeout time.Duration
}

// Load reads the configuration from the process environment.
func Load() (Config, error) {
	return LoadFrom(config.FromEnv())
}

// LoadFrom reads the configuration from l. It returns every problem at once.
func LoadFrom(l *config.Loader) (Config, error) {
	cfg := Config{
		Log:             logging.ConfigFromEnv(l, ServiceName),
		GRPCAddr:        l.String("GRPC_ADDR", ":9090"),
		HTTPAddr:        l.String("HTTP_ADDR", ":8080"),
		DatabaseURL:     l.RequiredString("DATABASE_URL"),
		NATSURL:         l.String("NATS_URL", "nats://localhost:4222"),
		SeedDemoData:    l.Bool("SEED_DEMO_DATA", false),
		ShutdownTimeout: l.Duration("SHUTDOWN_TIMEOUT", 15*time.Second),
	}
	if err := l.Err(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
