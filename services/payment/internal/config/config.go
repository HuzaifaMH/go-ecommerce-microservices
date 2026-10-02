// Package config loads and validates payment-service configuration from the environment.
package config

import (
	"time"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/config"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/logging"
)

// ServiceName identifies this service in logs and the NATS connection.
const ServiceName = "payment-service"

// Config is the complete runtime configuration.
type Config struct {
	Log             logging.Config
	GRPCAddr        string
	HTTPAddr        string // health endpoints
	DatabaseURL     string
	NATSURL         string
	ShutdownTimeout time.Duration
	// SimulatedMaxAmountMinor is the largest amount the simulated provider accepts.
	SimulatedMaxAmountMinor int64
}

// Load reads the configuration from the process environment.
func Load() (Config, error) {
	return LoadFrom(config.FromEnv())
}

// LoadFrom reads the configuration from l. It returns every problem at once.
func LoadFrom(l *config.Loader) (Config, error) {
	cfg := Config{
		Log:                     logging.ConfigFromEnv(l, ServiceName),
		GRPCAddr:                l.String("GRPC_ADDR", ":9090"),
		HTTPAddr:                l.String("HTTP_ADDR", ":8080"),
		DatabaseURL:             l.RequiredString("DATABASE_URL"),
		NATSURL:                 l.String("NATS_URL", "nats://localhost:4222"),
		ShutdownTimeout:         l.Duration("SHUTDOWN_TIMEOUT", 15*time.Second),
		SimulatedMaxAmountMinor: int64(l.Int("SIMULATED_MAX_AMOUNT_MINOR", 1_000_000)),
	}
	if err := l.Err(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
