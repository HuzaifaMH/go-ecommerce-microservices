package messaging

import (
	"context"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Outcome says what happened to a consumed message.
type Outcome string

const (
	// OutcomeAck: the handler succeeded.
	OutcomeAck Outcome = "ack"
	// OutcomeRetry: the handler failed and the message will be redelivered.
	OutcomeRetry Outcome = "retry"
	// OutcomeTerminated: the handler reported a permanent failure; the message is dropped.
	OutcomeTerminated Outcome = "terminated"
)

// Observer is told about every message consumed and published, for metrics.
type Observer interface {
	// Consumed is called once per delivery, after the handler has run.
	Consumed(consumer, subject string, outcome Outcome, took time.Duration)
	// Published is called once per publish attempt.
	Published(subject string, ok bool, took time.Duration)
}

type noopObserver struct{}

func (noopObserver) Consumed(string, string, Outcome, time.Duration) {}
func (noopObserver) Published(string, bool, time.Duration)           {}

func orNoop(o Observer) Observer {
	if o == nil {
		return noopObserver{}
	}
	return o
}

// headerCarrier lets the trace propagator read and write message headers.
// Lookups ignore case because NATS canonicalises header names ("traceparent"
// arrives as "Traceparent"), while the propagator asks for the lower-case form.
type headerCarrier map[string]string

func (c headerCarrier) Get(key string) string {
	if v, ok := c[key]; ok {
		return v
	}
	for k, v := range c {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

// Set replaces any existing header with the same name, whatever its case, so
// the message never carries two competing values.
func (c headerCarrier) Set(key, value string) {
	for k := range c {
		if strings.EqualFold(k, key) {
			delete(c, k)
		}
	}
	c[key] = value
}

func (c headerCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// tracerName identifies this package's spans.
const tracerName = "github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"

// tracer is fetched when needed, not once at start-up, so it always belongs to the
// provider that is installed now (a tracer taken at init would stay tied to the first one).
func tracer() trace.Tracer { return otel.Tracer(tracerName) }

// startPublishSpan starts a producer span for publishing m. Its parent is the
// trace context stored in the message headers when the message was created,
// so the publish shows up inside the trace of the request that caused it even
// though it happens later, on the outbox relay's goroutine. The returned
// headers carry the new span's context for the consumer.
func startPublishSpan(ctx context.Context, m Message) (context.Context, trace.Span, map[string]string) {
	prop := otel.GetTextMapPropagator()
	headers := make(map[string]string, len(m.Headers)+2)
	for k, v := range m.Headers {
		headers[k] = v
	}

	ctx = prop.Extract(ctx, headerCarrier(headers))
	ctx, span := tracer().Start(ctx, "publish "+m.Subject,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "nats"),
			attribute.String("messaging.destination.name", m.Subject),
			attribute.String("messaging.message.id", m.ID),
		))
	prop.Inject(ctx, headerCarrier(headers))
	return ctx, span, headers
}

// startConsumeSpan starts a consumer span for a delivered message, continuing
// the trace carried in its headers.
func startConsumeSpan(ctx context.Context, consumer string, m Message) (context.Context, trace.Span) {
	ctx = otel.GetTextMapPropagator().Extract(ctx, headerCarrier(m.Headers))
	return tracer().Start(ctx, "consume "+m.Subject,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "nats"),
			attribute.String("messaging.destination.name", m.Subject),
			attribute.String("messaging.message.id", m.ID),
			attribute.String("messaging.consumer", consumer),
		))
}

// markFailed records err on the span and marks it failed.
func markFailed(span trace.Span, err error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}
