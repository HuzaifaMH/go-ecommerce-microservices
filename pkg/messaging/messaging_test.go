package messaging

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"
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
