package config

import (
	"strings"
	"testing"
	"time"

	pkgconfig "github.com/HuzaifaMH/go-ecommerce-microservices/pkg/config"
)

func TestLoadFromDefaults(t *testing.T) {
	cfg, err := LoadFrom(pkgconfig.FromMap(map[string]string{"DATABASE_URL": "postgres://x"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GRPCAddr != ":9090" || cfg.HTTPAddr != ":8080" || cfg.NATSURL != "nats://localhost:4222" ||
		cfg.ShutdownTimeout != 15*time.Second || cfg.SimulatedMaxAmountMinor != 1_000_000 || cfg.Log.Service != ServiceName {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadFromOverrides(t *testing.T) {
	cfg, err := LoadFrom(pkgconfig.FromMap(map[string]string{
		"DATABASE_URL": "postgres://x", "SIMULATED_MAX_AMOUNT_MINOR": "500", "GRPC_ADDR": ":1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SimulatedMaxAmountMinor != 500 || cfg.GRPCAddr != ":1" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadFromReportsAllProblems(t *testing.T) {
	_, err := LoadFrom(pkgconfig.FromMap(map[string]string{"SIMULATED_MAX_AMOUNT_MINOR": "lots", "SHUTDOWN_TIMEOUT": "soon"}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"DATABASE_URL", "SIMULATED_MAX_AMOUNT_MINOR", "SHUTDOWN_TIMEOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}
