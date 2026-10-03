package observability

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/config"
)

// Instruments is everything a service needs to be observable, set up the same
// way in every service: tracing, a metrics registry, and the gRPC and
// messaging instrumentation that feeds them.
type Instruments struct {
	// Registry holds this process's metrics; serve it with WithMetrics.
	Registry *prometheus.Registry
	// Messaging observes consumers and publishers (pass to pkg/messaging).
	Messaging *MessagingObserver

	grpc     *GRPCMetrics
	shutdown func(context.Context) error
}

// Start configures tracing from the environment (OTEL_EXPORTER_OTLP_ENDPOINT,
// OTEL_TRACES_SAMPLE_RATIO) and creates the metrics registry for service.
// Call Close before the process exits so buffered spans are flushed.
func Start(ctx context.Context, service string) (*Instruments, error) {
	l := config.FromEnv()
	cfg := ConfigFromEnv(l, service)
	if err := l.Err(); err != nil {
		return nil, fmt.Errorf("observability configuration: %w", err)
	}
	shutdown, err := SetupTracing(ctx, cfg)
	if err != nil {
		return nil, err
	}

	reg := NewRegistry()
	return &Instruments{
		Registry:  reg,
		Messaging: NewMessagingObserver(reg),
		grpc:      NewGRPCMetrics(reg),
		shutdown:  shutdown,
	}, nil
}

// ServerOptions instrument a gRPC server: a span for every call (continuing
// the caller's trace) and RED metrics.
func (i *Instruments) ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(i.grpc.UnaryServerInterceptor()),
	}
}

// DialOptions instrument a gRPC client connection: a span for every call
// (passing the trace on to the server) and RED metrics for calls made.
func (i *Instruments) DialOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithChainUnaryInterceptor(i.grpc.UnaryClientInterceptor()),
	}
}

// Close flushes buffered spans and stops tracing, waiting at most timeout.
func (i *Instruments) Close(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return i.shutdown(ctx)
}
