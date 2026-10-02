// Package logging builds the structured logger used by every service.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/config"
)

// Config controls logger construction.
type Config struct {
	// Service is attached to every record as the "service" attribute.
	Service string
	// Level is one of debug, info, warn, error.
	Level string
	// Format is "json" (default, for production) or "text" (for local development).
	Format string
}

// ConfigFromEnv reads LOG_LEVEL and LOG_FORMAT.
func ConfigFromEnv(l *config.Loader, service string) Config {
	return Config{
		Service: service,
		Level:   l.String("LOG_LEVEL", "info"),
		Format:  l.String("LOG_FORMAT", "json"),
	}
}

// New returns a logger writing to w.
func New(w io.Writer, c Config) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToLower(c.Level))); err != nil {
		return nil, fmt.Errorf("invalid log level %q: %w", c.Level, err)
	}

	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	switch strings.ToLower(c.Format) {
	case "json", "":
		h = slog.NewJSONHandler(w, opts)
	case "text":
		h = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("invalid log format %q (want json or text)", c.Format)
	}

	return slog.New(h).With("service", c.Service), nil
}
