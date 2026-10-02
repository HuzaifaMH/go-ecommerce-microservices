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
		cfg.SeedDemoData || cfg.ShutdownTimeout != 15*time.Second || cfg.Log.Service != ServiceName {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadFromOverrides(t *testing.T) {
	cfg, err := LoadFrom(pkgconfig.FromMap(map[string]string{
		"DATABASE_URL": "postgres://x", "GRPC_ADDR": ":1", "SEED_DEMO_DATA": "true", "SHUTDOWN_TIMEOUT": "3s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GRPCAddr != ":1" || !cfg.SeedDemoData || cfg.ShutdownTimeout != 3*time.Second {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadFromReportsAllProblems(t *testing.T) {
	_, err := LoadFrom(pkgconfig.FromMap(map[string]string{"SEED_DEMO_DATA": "maybe", "SHUTDOWN_TIMEOUT": "soon"}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"DATABASE_URL", "SEED_DEMO_DATA", "SHUTDOWN_TIMEOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}
