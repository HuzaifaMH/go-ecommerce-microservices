package messaging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// startJetStream runs an in-process NATS server with JetStream enabled.
func startJetStream(t *testing.T) jetstream.JetStream {
	t.Helper()
	ns, err := server.NewServer(&server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(ns.Shutdown)

	nc, js, err := Connect(ns.ClientURL(), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return js
}

func setup(t *testing.T) (jetstream.JetStream, *JetStreamPublisher, jetstream.Stream) {
	t.Helper()
	js := startJetStream(t)
	stream, err := EnsureStream(context.Background(), js, StreamConfig{Name: "ORDERS", Subjects: []string{"order.>"}})
	if err != nil {
		t.Fatal(err)
	}
	return js, NewJetStreamPublisher(js), stream
}

func startConsumer(t *testing.T, js jetstream.JetStream, h Handler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Consume(ctx, js, ConsumerConfig{
			Stream: "ORDERS", Durable: "test", FilterSubjects: []string{"order.>"},
			RetryDelay: 20 * time.Millisecond, MaxDeliver: 5,
		}, discard, h)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Consume returned %v", err)
		}
	})
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestPublishRequiresID(t *testing.T) {
	_, pub, _ := setup(t)
	err := pub.Publish(context.Background(), Message{Subject: "order.created"})
	if !errors.Is(err, ErrMissingID) {
		t.Fatalf("err = %v, want ErrMissingID", err)
	}
}

func TestPublishDeduplicatesByMessageID(t *testing.T) {
	_, pub, stream := setup(t)
	ctx := context.Background()

	m := Message{ID: "msg-1", Subject: "order.created", Data: []byte("x")}
	for range 3 {
		if err := pub.Publish(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := pub.Publish(ctx, Message{ID: "msg-2", Subject: "order.created", Data: []byte("y")}); err != nil {
		t.Fatal(err)
	}

	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 2 {
		t.Fatalf("stream has %d messages, want 2 (duplicates must be dropped)", info.State.Msgs)
	}
}

func TestConsumeDeliversMessageWithIDAndHeaders(t *testing.T) {
	js, pub, _ := setup(t)

	got := make(chan Message, 1)
	startConsumer(t, js, func(_ context.Context, m Message) error {
		got <- m
		return nil
	})

	err := pub.Publish(context.Background(), Message{
		ID: "msg-1", Subject: "order.created", Data: []byte("payload"),
		Headers: map[string]string{HeaderCorrelationID: "corr-1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case m := <-got:
		if m.ID != "msg-1" || string(m.Data) != "payload" || m.Subject != "order.created" || m.Headers[HeaderCorrelationID] != "corr-1" {
			t.Fatalf("unexpected message: %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("message not delivered")
	}
}

func TestConsumePropagatesCorrelationID(t *testing.T) {
	js, pub, _ := setup(t)

	got := make(chan string, 1)
	startConsumer(t, js, func(ctx context.Context, _ Message) error {
		got <- CorrelationID(ctx)
		return nil
	})

	err := pub.Publish(context.Background(), Message{
		ID: "msg-1", Subject: "order.created", Headers: map[string]string{HeaderCorrelationID: "corr-42"},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-got:
		if id != "corr-42" {
			t.Fatalf("CorrelationID = %q, want corr-42", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("message not delivered")
	}
}

func TestCorrelationIDDefaultsToEmpty(t *testing.T) {
	if id := CorrelationID(context.Background()); id != "" {
		t.Fatalf("CorrelationID = %q, want empty", id)
	}
}

func TestConsumeRetriesAfterError(t *testing.T) {
	js, pub, _ := setup(t)

	var calls atomic.Int32
	startConsumer(t, js, func(context.Context, Message) error {
		if calls.Add(1) == 1 {
			return errors.New("transient")
		}
		return nil
	})

	if err := pub.Publish(context.Background(), Message{ID: "msg-1", Subject: "order.created"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return calls.Load() >= 2 })
}

func TestConsumeRetriesAfterPanic(t *testing.T) {
	js, pub, _ := setup(t)

	var calls atomic.Int32
	startConsumer(t, js, func(context.Context, Message) error {
		if calls.Add(1) == 1 {
			panic("bug")
		}
		return nil
	})

	if err := pub.Publish(context.Background(), Message{ID: "msg-1", Subject: "order.created"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return calls.Load() >= 2 })
}

func TestConsumeDoesNotRetryPermanentErrors(t *testing.T) {
	js, pub, _ := setup(t)

	var calls atomic.Int32
	startConsumer(t, js, func(context.Context, Message) error {
		calls.Add(1)
		return errors.Join(ErrPermanent, errors.New("bad payload"))
	})

	if err := pub.Publish(context.Background(), Message{ID: "msg-1", Subject: "order.created"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return calls.Load() == 1 })
	time.Sleep(300 * time.Millisecond) // several retry delays
	if n := calls.Load(); n != 1 {
		t.Fatalf("handler called %d times, want 1", n)
	}
}

type memInbox struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (i *memInbox) Once(ctx context.Context, consumer, id string, fn func(context.Context) error) error {
	i.mu.Lock()
	key := consumer + "/" + id
	if i.seen[key] {
		i.mu.Unlock()
		return nil
	}
	i.mu.Unlock()
	if err := fn(ctx); err != nil {
		return err
	}
	i.mu.Lock()
	i.seen[key] = true
	i.mu.Unlock()
	return nil
}

func TestIdempotentSkipsDuplicates(t *testing.T) {
	var calls int
	h := Idempotent("inventory", &memInbox{seen: map[string]bool{}}, func(context.Context, Message) error {
		calls++
		return nil
	})

	ctx := context.Background()
	for _, id := range []string{"a", "a", "b", "a"} {
		if err := h(ctx, Message{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("handler ran %d times, want 2", calls)
	}
}

// --- tracing and observers ---

// recordTraces installs a tracer provider that keeps spans in memory, and the
// W3C propagator, for the duration of the test.
func recordTraces(t *testing.T) (*tracetest.SpanRecorder, trace.Tracer) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	return rec, tp.Tracer("test")
}

// The request that creates a message has long finished by the time the outbox
// relay publishes it, yet the consumer's work must still belong to that
// request's trace. This runs over a real NATS server, which canonicalises
// header names ("traceparent" -> "Traceparent"); a case-sensitive carrier
// would silently break the trace here.
func TestTraceSurvivesDelayedPublishAndBrokerHeaderRewriting(t *testing.T) {
	rec, tr := recordTraces(t)
	js, pub, _ := setup(t)

	handlerCtx := make(chan context.Context, 1)
	startConsumer(t, js, func(ctx context.Context, _ Message) error {
		handlerCtx <- ctx
		return nil
	})

	// 1. A request creates the message (as the outbox does) and finishes.
	reqCtx, request := tr.Start(context.Background(), "request")
	msg, err := NewProtoMessage(reqCtx, "order.created", wrapperspb.String("x"))
	if err != nil {
		t.Fatal(err)
	}
	msg.ID = "m-1"
	request.End()
	if msg.Headers["traceparent"] == "" {
		t.Fatalf("the trace context must be stored in the message headers: %v", msg.Headers)
	}

	// 2. Later, the relay publishes it from a context that knows nothing about the request.
	if err := pub.Publish(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	// 3. The consumer's handler runs inside the original trace.
	select {
	case ctx := <-handlerCtx:
		want := request.SpanContext().TraceID()
		if got := trace.SpanContextFromContext(ctx).TraceID(); got != want {
			t.Fatalf("handler ran in trace %s, want the request's trace %s", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("message not delivered")
	}

	eventually(t, func() bool { return len(rec.Ended()) == 3 })
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range rec.Ended() {
		byName[s.Name()] = s
	}
	publish, consume := byName["publish order.created"], byName["consume order.created"]
	if publish == nil || consume == nil {
		t.Fatalf("spans = %v", rec.Ended())
	}
	if publish.Parent().SpanID() != request.SpanContext().SpanID() || publish.SpanKind() != trace.SpanKindProducer {
		t.Errorf("publish span: parent %s kind %v, want a producer child of the request", publish.Parent().SpanID(), publish.SpanKind())
	}
	if consume.Parent().SpanID() != publish.SpanContext().SpanID() || consume.SpanKind() != trace.SpanKindConsumer {
		t.Errorf("consume span: parent %s kind %v, want a consumer child of the publish span", consume.Parent().SpanID(), consume.SpanKind())
	}
	if consume.SpanContext().TraceID() != request.SpanContext().TraceID() {
		t.Error("all three spans must share one trace")
	}
}

func TestAFailedHandlerMarksTheConsumeSpanFailed(t *testing.T) {
	rec, _ := recordTraces(t)
	js, pub, _ := setup(t)
	startConsumer(t, js, func(context.Context, Message) error { return errors.Join(ErrPermanent, errors.New("bad payload")) })

	if err := pub.Publish(context.Background(), Message{ID: "m-1", Subject: "order.created"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		for _, s := range rec.Ended() {
			if s.Name() == "consume order.created" {
				return true
			}
		}
		return false
	})
	for _, s := range rec.Ended() {
		if s.Name() == "consume order.created" && s.Status().Code.String() != "Error" {
			t.Fatalf("status = %v, want Error", s.Status())
		}
	}
}

func TestHeaderCarrierIgnoresCaseAndNeverKeepsDuplicates(t *testing.T) {
	c := headerCarrier{"Traceparent": "00-aaa-bbb-01", "Other": "x"}
	if got := c.Get("traceparent"); got != "00-aaa-bbb-01" {
		t.Errorf("Get = %q: lookups must ignore case", got)
	}
	if got := c.Get("missing"); got != "" {
		t.Errorf("Get(missing) = %q", got)
	}

	c.Set("traceparent", "new")
	if len(c) != 2 || c["traceparent"] != "new" {
		t.Fatalf("carrier = %v: Set must replace the differently-cased header, not add a second one", c)
	}
	if len(c.Keys()) != 2 {
		t.Errorf("Keys = %v", c.Keys())
	}
}

type recordingObserver struct {
	mu        sync.Mutex
	consumed  []string
	published []string
}

func (r *recordingObserver) Consumed(consumer, subject string, outcome Outcome, took time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if took < 0 {
		panic("negative duration")
	}
	r.consumed = append(r.consumed, consumer+"/"+subject+"/"+string(outcome))
}

func (r *recordingObserver) Published(subject string, ok bool, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.published = append(r.published, fmt.Sprintf("%s/%v", subject, ok))
}

func (r *recordingObserver) snapshot() (consumed, published []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.consumed...), append([]string(nil), r.published...)
}

func TestObserverSeesEveryDeliveryOutcomeAndPublish(t *testing.T) {
	js := startJetStream(t)
	if _, err := EnsureStream(context.Background(), js, StreamConfig{Name: "ORDERS", Subjects: []string{"order.>"}}); err != nil {
		t.Fatal(err)
	}
	obs := &recordingObserver{}
	pub := NewJetStreamPublisher(js, WithObserver(obs))

	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = Consume(ctx, js, ConsumerConfig{
			Stream: "ORDERS", Durable: "test", FilterSubjects: []string{"order.>"},
			RetryDelay: 10 * time.Millisecond, MaxDeliver: 5, Observer: obs,
		}, discard, func(_ context.Context, m Message) error {
			switch m.Subject {
			case "order.ok":
				return nil
			case "order.bad":
				return errors.Join(ErrPermanent, errors.New("poison"))
			default: // order.flaky: fails once, then succeeds
				if calls.Add(1) == 1 {
					return errors.New("transient")
				}
				return nil
			}
		})
	}()

	for _, subject := range []string{"order.ok", "order.bad", "order.flaky"} {
		if err := pub.Publish(context.Background(), Message{ID: "id-" + subject, Subject: subject}); err != nil {
			t.Fatal(err)
		}
	}
	if err := pub.Publish(context.Background(), Message{Subject: "order.nope"}); !errors.Is(err, ErrMissingID) { // no ID
		t.Fatalf("err = %v", err)
	}

	eventually(t, func() bool { c, _ := obs.snapshot(); return len(c) >= 4 })
	consumed, published := obs.snapshot()

	has := func(list []string, want string) bool {
		for _, s := range list {
			if s == want {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"test/order.ok/ack", "test/order.bad/terminated", "test/order.flaky/retry", "test/order.flaky/ack"} {
		if !has(consumed, want) {
			t.Errorf("consumed = %v, missing %s", consumed, want)
		}
	}
	for _, want := range []string{"order.ok/true", "order.bad/true", "order.flaky/true", "order.nope/false"} {
		if !has(published, want) {
			t.Errorf("published = %v, missing %s", published, want)
		}
	}
}
