package config

import (
	"strings"
	"testing"
	"time"

	pkgconfig "github.com/HuzaifaMH/go-ecommerce-microservices/pkg/config"
)

func load(env map[string]string) (Config, error) {
	return LoadFrom(pkgconfig.FromMap(env))
}

func TestDevAuthNeedsNoOtherAuthSettings(t *testing.T) {
	cfg, err := load(map[string]string{"DEV_AUTH": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DevAuth || cfg.JWTIssuer != "ecommerce-dev" || cfg.JWTAudience != "ecommerce-api" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.HTTPAddr != ":8080" || cfg.OrderAddr != "localhost:9092" || cfg.InventoryAddr != "localhost:9090" || cfg.NotificationAddr != "localhost:9093" ||
		cfg.RateLimitRPS != 20 || cfg.RateLimitBurst != 40 || cfg.MaxBodyBytes != 1<<20 || cfg.BackendTimeout != 5*time.Second || cfg.ShutdownTimeout != 15*time.Second {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestProductionRequiresAWayToVerifyTokens(t *testing.T) {
	_, err := load(map[string]string{})
	if err == nil {
		t.Fatal("a gateway with no token verification must not start")
	}
	for _, want := range []string{"JWT_ISSUER is required", "JWT_AUDIENCE is required", "JWT_PUBLIC_KEY_FILE or JWT_JWKS_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestExactlyOneKeySource(t *testing.T) {
	base := map[string]string{"JWT_ISSUER": "https://idp.example", "JWT_AUDIENCE": "api"}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	if _, err := load(with(map[string]string{"JWT_PUBLIC_KEY_FILE": "/keys/public.pem"})); err != nil {
		t.Errorf("a public key file: %v", err)
	}
	if _, err := load(with(map[string]string{"JWT_JWKS_URL": "https://idp.example/.well-known/jwks.json"})); err != nil {
		t.Errorf("a JWKS URL: %v", err)
	}
	if _, err := load(with(map[string]string{"JWT_PUBLIC_KEY_FILE": "/k", "JWT_JWKS_URL": "https://idp.example/jwks"})); err == nil ||
		!strings.Contains(err.Error(), "only one") {
		t.Errorf("both sources: err = %v", err)
	}
}

func TestDevAuthCannotBeMixedWithRealKeys(t *testing.T) {
	_, err := load(map[string]string{"DEV_AUTH": "true", "JWT_JWKS_URL": "https://idp.example/jwks"})
	if err == nil || !strings.Contains(err.Error(), "DEV_AUTH cannot be combined") {
		t.Fatalf("err = %v", err)
	}
}

func TestJWKSURLMustBeHTTPSExceptForLocalhost(t *testing.T) {
	base := map[string]string{"JWT_ISSUER": "i", "JWT_AUDIENCE": "a"}
	for url, ok := range map[string]bool{
		"https://idp.example/jwks":   true,
		"http://localhost:8081/jwks": true,
		"http://127.0.0.1:8081/jwks": true,
		"http://idp.example/jwks":    false, // could be swapped in transit
		"ftp://idp.example/jwks":     false,
		"/relative/path":             false,
		"https://":                   false,
	} {
		env := map[string]string{"JWT_JWKS_URL": url}
		for k, v := range base {
			env[k] = v
		}
		_, err := load(env)
		if (err == nil) != ok {
			t.Errorf("%s: err = %v, want ok=%v", url, err, ok)
		}
	}
}

func TestCORSOriginsAreValidated(t *testing.T) {
	good, err := load(map[string]string{"DEV_AUTH": "true", "CORS_ALLOWED_ORIGINS": "https://admin.example, http://localhost:4200"})
	if err != nil || len(good.CORSOrigins) != 2 {
		t.Fatalf("cfg = %+v, err = %v", good, err)
	}
	if _, err := load(map[string]string{"DEV_AUTH": "true", "CORS_ALLOWED_ORIGINS": "*"}); err != nil {
		t.Errorf("wildcard: %v", err)
	}
	for _, bad := range []string{"admin.example", "https://admin.example/path", "javascript:alert(1)", "https://"} {
		if _, err := load(map[string]string{"DEV_AUTH": "true", "CORS_ALLOWED_ORIGINS": bad}); err == nil {
			t.Errorf("origin %q must be rejected", bad)
		}
	}
}

func TestLimitsAreValidated(t *testing.T) {
	dev := func(extra map[string]string) (Config, error) {
		env := map[string]string{"DEV_AUTH": "true"}
		for k, v := range extra {
			env[k] = v
		}
		return load(env)
	}

	if cfg, err := dev(map[string]string{"RATE_LIMIT_RPS": "0"}); err != nil || cfg.RateLimitRPS != 0 {
		t.Errorf("rate limiting can be turned off: %+v %v", cfg, err)
	}
	if _, err := dev(map[string]string{"RATE_LIMIT_RPS": "-1"}); err == nil {
		t.Error("negative rate must be rejected")
	}
	if _, err := dev(map[string]string{"RATE_LIMIT_RPS": "5", "RATE_LIMIT_BURST": "0"}); err == nil {
		t.Error("a burst below 1 with limiting on must be rejected")
	}
	if _, err := dev(map[string]string{"MAX_BODY_BYTES": "0"}); err == nil {
		t.Error("a zero body limit must be rejected")
	}
	if _, err := dev(map[string]string{"BACKEND_TIMEOUT": "0s"}); err == nil {
		t.Error("a zero backend timeout must be rejected")
	}
}

func TestParseErrorsAreReportedTogether(t *testing.T) {
	_, err := load(map[string]string{"DEV_AUTH": "maybe", "RATE_LIMIT_RPS": "fast", "SHUTDOWN_TIMEOUT": "soon"})
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"DEV_AUTH", "RATE_LIMIT_RPS", "SHUTDOWN_TIMEOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}
