package observability

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// NewRegistry returns the metrics registry for one process. It is separate
// from Prometheus' global default so that only the metrics this system
// defines are exposed, and so tests can create as many as they like.
func NewRegistry() *prometheus.Registry {
	return prometheus.NewRegistry()
}

// MetricsHandler serves the registry in the Prometheus text format.
func MetricsHandler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})
}

// WithMetrics returns a handler that serves /metrics from reg and sends every
// other request to next (typically the health endpoints). Use it on internal
// listeners only: metrics must not be exposed to the public internet.
func WithMetrics(next http.Handler, reg *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", MetricsHandler(reg))
	mux.Handle("/", next)
	return mux
}

// latencyBuckets suit request latencies from a few milliseconds to several seconds.
var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// GRPCMetrics records rate, errors and duration of gRPC calls: the standard
// RED metrics, for the calls a service serves and the calls it makes.
type GRPCMetrics struct {
	serverHandled  *prometheus.CounterVec
	serverDuration *prometheus.HistogramVec
	clientHandled  *prometheus.CounterVec
	clientDuration *prometheus.HistogramVec
}

// NewGRPCMetrics registers the gRPC metrics on reg.
func NewGRPCMetrics(reg prometheus.Registerer) *GRPCMetrics {
	f := promauto.With(reg)
	return &GRPCMetrics{
		serverHandled: f.NewCounterVec(prometheus.CounterOpts{
			Name: "grpc_server_handled_total",
			Help: "gRPC calls served, by method and result code.",
		}, []string{"grpc_method", "grpc_code"}),
		serverDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "grpc_server_handling_seconds", Help: "Time spent serving gRPC calls.", Buckets: latencyBuckets,
		}, []string{"grpc_method"}),
		clientHandled: f.NewCounterVec(prometheus.CounterOpts{
			Name: "grpc_client_handled_total",
			Help: "gRPC calls made to other services, by method and result code.",
		}, []string{"grpc_method", "grpc_code"}),
		clientDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "grpc_client_handling_seconds", Help: "Time spent on gRPC calls made to other services.", Buckets: latencyBuckets,
		}, []string{"grpc_method"}),
	}
}

// UnaryServerInterceptor records every unary call the server handles.
func (m *GRPCMetrics) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		m.serverHandled.WithLabelValues(info.FullMethod, status.Code(err).String()).Inc()
		m.serverDuration.WithLabelValues(info.FullMethod).Observe(time.Since(start).Seconds())
		return resp, err
	}
}

// UnaryClientInterceptor records every unary call the client makes.
func (m *GRPCMetrics) UnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		start := time.Now()
		err := invoker(ctx, method, req, reply, cc, opts...)
		m.clientHandled.WithLabelValues(method, status.Code(err).String()).Inc()
		m.clientDuration.WithLabelValues(method).Observe(time.Since(start).Seconds())
		return err
	}
}
