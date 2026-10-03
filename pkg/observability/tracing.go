// Package observability sets up distributed tracing and Prometheus metrics
// the same way for every service.
//
// Tracing uses OpenTelemetry. Context travels between services in W3C
// "traceparent" headers: over gRPC through the otelgrpc handlers, and over
// JetStream through message headers (see pkg/messaging). Because messages are
// published from the outbox long after the request that caused them, the
// trace context is stored with each outbox row, so an order still shows up as
// one connected trace across the whole saga.
//
// Metrics use the Prometheus client, with one registry per process, served
// on /metrics.
package observability

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/config"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/version"
)

// Config controls tracing.
type Config struct {
	// Service is reported as the trace's service name.
	Service string
	// Version is reported as the service version.
	Version string
	// OTLPEndpoint is where spans are sent over OTLP/gRPC, e.g. "http://jaeger:4317".
	// Empty disables exporting: spans are still created and context is still
	// propagated, but nothing is sent anywhere.
	OTLPEndpoint string
	// SampleRatio is the fraction of new traces to record, between 0 and 1.
	// A trace that arrives already sampled stays sampled.
	SampleRatio float64
}

// ConfigFromEnv reads OTEL_EXPORTER_OTLP_ENDPOINT and OTEL_TRACES_SAMPLE_RATIO.
func ConfigFromEnv(l *config.Loader, service string) Config {
	return Config{
		Service:      service,
		Version:      version.Version,
		OTLPEndpoint: l.String("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		SampleRatio:  l.Float("OTEL_TRACES_SAMPLE_RATIO", 1),
	}
}

// SetupTracing installs the global propagator and tracer provider and returns
// a function that flushes and stops tracing; call it before the process exits.
func SetupTracing(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	// Always propagate, even when nothing is exported, so a trace that passes
	// through this service is not broken.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	if cfg.OTLPEndpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	if cfg.SampleRatio < 0 || cfg.SampleRatio > 1 {
		return nil, fmt.Errorf("OTEL_TRACES_SAMPLE_RATIO must be between 0 and 1, got %v", cfg.SampleRatio)
	}

	exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpointURL(cfg.OTLPEndpoint))
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}
	res := resource.NewSchemaless(
		attribute.String("service.name", cfg.Service),
		attribute.String("service.version", cfg.Version),
	)
	tp := sdktrace.NewTracerProvider(
		// A short batch timeout keeps traces visible within about a second.
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(time.Second)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	)
	otel.SetTracerProvider(tp)

	return func(ctx context.Context) error {
		if err := tp.Shutdown(ctx); err != nil {
			return fmt.Errorf("shut down tracing: %w", err)
		}
		return nil
	}, nil
}

// SetSpanAttrs adds string attributes (key, value, key, value, ...) to the
// span in ctx, such as order.id, so a trace can be found by business ID. It
// does nothing when there is no recording span.
func SetSpanAttrs(ctx context.Context, keyValues ...string) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	attrs := make([]attribute.KeyValue, 0, len(keyValues)/2)
	for i := 0; i+1 < len(keyValues); i += 2 {
		attrs = append(attrs, attribute.String(keyValues[i], keyValues[i+1]))
	}
	span.SetAttributes(attrs...)
}

// RecordError marks the span in ctx as failed with err. It does nothing when
// err is nil or there is no recording span.
func RecordError(ctx context.Context, err error) {
	if err == nil {
		return
	}
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// TraceID returns the ID of the trace in ctx, or "" when there is none.
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}
