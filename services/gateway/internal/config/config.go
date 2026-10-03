// Package config loads and validates api-gateway configuration from the environment.
//
// The gateway is the only service exposed to the outside, so its configuration
// is validated strictly: a deployment that would run without a way to verify
// tokens fails at start-up instead of silently running open or locked.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/config"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/logging"
)

// ServiceName identifies this service in logs.
const ServiceName = "api-gateway"

// Development defaults, used only when DEV_AUTH is on.
const (
	devIssuer   = "ecommerce-dev"
	devAudience = "ecommerce-api"
)

// Config is the complete runtime configuration.
type Config struct {
	Log             logging.Config
	HTTPAddr        string
	ShutdownTimeout time.Duration

	// MetricsAddr is the internal address serving /metrics. It must differ from
	// HTTPAddr: HTTPAddr is public, metrics must not be.
	MetricsAddr string

	// Backend services (gRPC).
	OrderAddr        string
	InventoryAddr    string
	NotificationAddr string
	// BackendTimeout is the deadline given to each backend call.
	BackendTimeout time.Duration

	// Authentication. Exactly one key source is used: DevAuth, a PEM public
	// key file, or a JWKS URL.
	JWTIssuer        string
	JWTAudience      string
	JWTPublicKeyFile string
	JWKSURL          string
	// DevAuth generates a throwaway key pair and enables POST /dev/token.
	// Local development only.
	DevAuth bool

	CORSOrigins []string

	// RateLimitRPS is the sustained requests per second per caller; 0 disables limiting.
	RateLimitRPS   float64
	RateLimitBurst int

	MaxBodyBytes int64
}

// Load reads the configuration from the process environment.
func Load() (Config, error) {
	return LoadFrom(config.FromEnv())
}

// LoadFrom reads the configuration from l. It returns every problem at once.
func LoadFrom(l *config.Loader) (Config, error) {
	cfg := Config{
		Log:              logging.ConfigFromEnv(l, ServiceName),
		HTTPAddr:         l.String("HTTP_ADDR", ":8080"),
		MetricsAddr:      l.String("METRICS_ADDR", ":9100"),
		ShutdownTimeout:  l.Duration("SHUTDOWN_TIMEOUT", 15*time.Second),
		OrderAddr:        l.String("ORDER_GRPC_ADDR", "localhost:9092"),
		InventoryAddr:    l.String("INVENTORY_GRPC_ADDR", "localhost:9090"),
		NotificationAddr: l.String("NOTIFICATION_GRPC_ADDR", "localhost:9093"),
		BackendTimeout:   l.Duration("BACKEND_TIMEOUT", 5*time.Second),
		JWTIssuer:        l.String("JWT_ISSUER", ""),
		JWTAudience:      l.String("JWT_AUDIENCE", ""),
		JWTPublicKeyFile: l.String("JWT_PUBLIC_KEY_FILE", ""),
		JWKSURL:          l.String("JWT_JWKS_URL", ""),
		DevAuth:          l.Bool("DEV_AUTH", false),
		CORSOrigins:      l.Strings("CORS_ALLOWED_ORIGINS"),
		RateLimitRPS:     l.Float("RATE_LIMIT_RPS", 20),
		RateLimitBurst:   l.Int("RATE_LIMIT_BURST", 40),
		MaxBodyBytes:     int64(l.Int("MAX_BODY_BYTES", 1<<20)),
	}
	if err := l.Err(); err != nil {
		return Config{}, err
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.DevAuth {
		// Development: the gateway mints its own tokens, so no external key may be configured.
		if c.JWTPublicKeyFile != "" || c.JWKSURL != "" {
			add("DEV_AUTH cannot be combined with JWT_PUBLIC_KEY_FILE or JWT_JWKS_URL")
		}
		if c.JWTIssuer == "" {
			c.JWTIssuer = devIssuer
		}
		if c.JWTAudience == "" {
			c.JWTAudience = devAudience
		}
	} else {
		if c.JWTIssuer == "" {
			add("JWT_ISSUER is required")
		}
		if c.JWTAudience == "" {
			add("JWT_AUDIENCE is required")
		}
		switch {
		case c.JWTPublicKeyFile == "" && c.JWKSURL == "":
			add("set JWT_PUBLIC_KEY_FILE or JWT_JWKS_URL so tokens can be verified (or DEV_AUTH=true for local development)")
		case c.JWTPublicKeyFile != "" && c.JWKSURL != "":
			add("set only one of JWT_PUBLIC_KEY_FILE and JWT_JWKS_URL")
		}
	}

	if c.JWKSURL != "" {
		if err := checkJWKSURL(c.JWKSURL); err != nil {
			add("JWT_JWKS_URL: %v", err)
		}
	}
	for _, origin := range c.CORSOrigins {
		if origin == "*" {
			continue
		}
		if u, err := url.Parse(origin); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" {
			add("CORS_ALLOWED_ORIGINS: %q is not an origin like https://admin.example", origin)
		}
	}
	if c.MetricsAddr == "" || c.MetricsAddr == c.HTTPAddr {
		add("METRICS_ADDR must be set and different from HTTP_ADDR: the public port must never serve /metrics")
	}
	if c.RateLimitRPS < 0 {
		add("RATE_LIMIT_RPS must not be negative")
	}
	if c.RateLimitRPS > 0 && c.RateLimitBurst < 1 {
		add("RATE_LIMIT_BURST must be at least 1 when rate limiting is on")
	}
	if c.MaxBodyBytes < 1 {
		add("MAX_BODY_BYTES must be positive")
	}
	if c.BackendTimeout <= 0 {
		add("BACKEND_TIMEOUT must be positive")
	}
	return errors.Join(errs...)
}

// checkJWKSURL requires HTTPS: a JWKS fetched over plain HTTP could be
// replaced in transit by an attacker's key. Plain HTTP is accepted only for
// localhost, for tests and local development.
func checkJWKSURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("must be an absolute URL")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if h := u.Hostname(); h == "localhost" || h == "127.0.0.1" || h == "::1" {
			return nil
		}
		return errors.New("must use https (http is only accepted for localhost)")
	default:
		return errors.New("must use https")
	}
}
