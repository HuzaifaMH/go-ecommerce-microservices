package outbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeStore hands out queued records on each Dispatch, like a real store would.
type fakeStore struct {
	mu      sync.Mutex
	pending []Record
	calls   int
	failFor int // number of initial Dispatch calls that return an error
}

func (f *fakeStore) Dispatch(ctx context.Context, limit int, publish func(context.Context, Record) error) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failFor {
		return 0, errors.New("database unavailable")
	}
	n := 0
	for n < len(f.pending) && n < limit {
		if err := publish(ctx, f.pending[n]); err != nil {
			f.pending = f.pending[n:]
			return n, err
		}
		n++
	}
	f.pending = f.pending[n:]
	return n, nil
}

type recordingPublisher struct {
	mu   sync.Mutex
	msgs []messaging.Message
}

func (p *recordingPublisher) Publish(_ context.Context, m messaging.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, m)
	return nil
}

func (p *recordingPublisher) ids() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.msgs))
	for i, m := range p.msgs {
		out[i] = m.ID
	}
	return out
}

func runRelay(t *testing.T, store Store, pub messaging.Publisher, opts Options) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewRelay(store, pub, discard, opts).Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Run = %v, want context.Canceled", err)
			}
		case <-time.After(time.Second):
			t.Error("relay did not stop")
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestRelayPublishesPendingRecordsInOrder(t *testing.T) {
	store := &fakeStore{pending: []Record{
		{ID: "a", Subject: "s.one", Payload: []byte("1")},
		{ID: "b", Subject: "s.two", Payload: []byte("2")},
		{ID: "c", Subject: "s.three", Payload: []byte("3")},
	}}
	pub := &recordingPublisher{}

	stop := runRelay(t, store, pub, Options{BatchSize: 2, PollInterval: 5 * time.Millisecond})
	defer stop()

	waitFor(t, func() bool { return len(pub.ids()) == 3 })
	got := pub.ids()
	for i, want := range []string{"a", "b", "c"} {
		if got[i] != want {
			t.Fatalf("published order = %v", got)
		}
	}
	if m := pub.msgs[0]; m.Subject != "s.one" || string(m.Data) != "1" {
		t.Errorf("record not mapped to message: %+v", m)
	}
}

func TestRelayRecoversAfterStoreErrors(t *testing.T) {
	store := &fakeStore{failFor: 2, pending: []Record{{ID: "a", Subject: "s"}}}
	pub := &recordingPublisher{}

	stop := runRelay(t, store, pub, Options{PollInterval: 5 * time.Millisecond, ErrorBackoff: 5 * time.Millisecond})
	defer stop()

	waitFor(t, func() bool { return len(pub.ids()) == 1 })
}

type failingPublisher struct{ err error }

func (p failingPublisher) Publish(context.Context, messaging.Message) error { return p.err }

func TestRelayKeepsRecordsPendingWhenPublishFails(t *testing.T) {
	store := &fakeStore{pending: []Record{{ID: "a", Subject: "s"}}}

	stop := runRelay(t, store, failingPublisher{errors.New("broker down")}, Options{PollInterval: 5 * time.Millisecond, ErrorBackoff: 5 * time.Millisecond})
	waitFor(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.calls >= 2
	})
	stop()

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.pending) != 1 {
		t.Fatalf("record should stay pending, have %d", len(store.pending))
	}
}
