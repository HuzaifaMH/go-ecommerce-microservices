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
	if cfg.GRPCAddr != ":9090" || cfg.InventoryAddr != "localhost:9090" || cfg.InventoryTimeout != 3*time.Second ||
		cfg.SagaTimeout != 2*time.Minute || cfg.SweepInterval != 10*time.Second || cfg.Log.Service != ServiceName {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadFromOverrides(t *testing.T) {
	cfg, err := LoadFrom(pkgconfig.FromMap(map[string]string{
		"DATABASE_URL": "postgres://x", "INVENTORY_GRPC_ADDR": "inventory:9090", "SAGA_TIMEOUT": "30s", "SWEEP_INTERVAL": "1s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InventoryAddr != "inventory:9090" || cfg.SagaTimeout != 30*time.Second || cfg.SweepInterval != time.Second {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadFromReportsAllProblems(t *testing.T) {
	_, err := LoadFrom(pkgconfig.FromMap(map[string]string{"SAGA_TIMEOUT": "soon", "SWEEP_INTERVAL": "often"}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"DATABASE_URL", "SAGA_TIMEOUT", "SWEEP_INTERVAL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestLoadFromRejectsNonPositiveDurations(t *testing.T) {
	_, err := LoadFrom(pkgconfig.FromMap(map[string]string{
		"DATABASE_URL": "postgres://x", "SAGA_TIMEOUT": "0s", "SWEEP_INTERVAL": "-5s", "INVENTORY_TIMEOUT": "0s",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"SAGA_TIMEOUT must be positive", "SWEEP_INTERVAL must be positive", "INVENTORY_TIMEOUT must be positive"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
