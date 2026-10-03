package pgstore_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/outbox"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/outbox/pgstore"
)

// newStore starts a throwaway Postgres container. The test is skipped with
// -short or when Docker is not available.
func newStore(t *testing.T) (*pgstore.Store, *pgxpool.Pool) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped with -short")
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, pgstore.Schema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return pgstore.New(pool), pool
}

func enqueue(t *testing.T, s *pgstore.Store, ids ...string) {
	t.Helper()
	err := s.WithTx(context.Background(), func(ctx context.Context) error {
		for _, id := range ids {
			if _, err := s.Enqueue(ctx, messaging.Message{ID: id, Subject: "s." + id, Data: []byte(id), Headers: map[string]string{"k": "v"}}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, where string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM outbox WHERE "+where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEnqueueRequiresTransaction(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.Enqueue(context.Background(), messaging.Message{Subject: "s"}); !errors.Is(err, pgstore.ErrNoTx) {
		t.Fatalf("err = %v, want ErrNoTx", err)
	}
}

func TestEnqueueRollsBackWithTransaction(t *testing.T) {
	s, pool := newStore(t)

	boom := errors.New("boom")
	err := s.WithTx(context.Background(), func(ctx context.Context) error {
		if _, err := s.Enqueue(ctx, messaging.Message{Subject: "s"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if n := count(t, pool, "true"); n != 0 {
		t.Fatalf("outbox has %d rows after rollback, want 0", n)
	}
}

func TestEnqueueGeneratesID(t *testing.T) {
	s, _ := newStore(t)
	var id string
	err := s.WithTx(context.Background(), func(ctx context.Context) error {
		var err error
		id, err = s.Enqueue(ctx, messaging.Message{Subject: "s"})
		return err
	})
	if err != nil || id == "" {
		t.Fatalf("id = %q, err = %v", id, err)
	}
}

func TestDispatchPublishesInOrderAndMarksPublished(t *testing.T) {
	s, pool := newStore(t)
	enqueue(t, s, "a", "b", "c")

	var got []outbox.Record
	n, err := s.Dispatch(context.Background(), 10, func(_ context.Context, r outbox.Record) error {
		got = append(got, r)
		return nil
	})
	if err != nil || n != 3 {
		t.Fatalf("Dispatch = %d, %v", n, err)
	}
	if got[0].ID != "a" || got[2].ID != "c" || got[0].Headers["k"] != "v" || string(got[1].Payload) != "b" || got[1].Subject != "s.b" {
		t.Fatalf("unexpected records: %+v", got)
	}

	if n, err := s.Dispatch(context.Background(), 10, func(context.Context, outbox.Record) error { return nil }); err != nil || n != 0 {
		t.Fatalf("second Dispatch = %d, %v; want nothing left", n, err)
	}
	if n := count(t, pool, "published_at IS NULL"); n != 0 {
		t.Fatalf("%d rows still pending", n)
	}
}

func TestDispatchStopsAtFirstFailureAndKeepsRestPending(t *testing.T) {
	s, pool := newStore(t)
	enqueue(t, s, "a", "b", "c")

	boom := errors.New("broker down")
	n, err := s.Dispatch(context.Background(), 10, func(_ context.Context, r outbox.Record) error {
		if r.ID == "b" {
			return boom
		}
		return nil
	})
	if n != 1 || !errors.Is(err, boom) {
		t.Fatalf("Dispatch = %d, %v; want 1, boom", n, err)
	}
	if got := count(t, pool, "published_at IS NULL"); got != 2 {
		t.Fatalf("%d rows pending, want 2 (b and c)", got)
	}
}

func TestDispatchHonoursLimit(t *testing.T) {
	s, pool := newStore(t)
	enqueue(t, s, "a", "b", "c")

	n, err := s.Dispatch(context.Background(), 2, func(context.Context, outbox.Record) error { return nil })
	if err != nil || n != 2 {
		t.Fatalf("Dispatch = %d, %v", n, err)
	}
	if got := count(t, pool, "published_at IS NULL"); got != 1 {
		t.Fatalf("%d rows pending, want 1", got)
	}
}

func TestConcurrentRelaysNeverPublishARowTwice(t *testing.T) {
	s, _ := newStore(t)
	const total = 60
	ids := make([]string, total)
	for i := range ids {
		ids[i] = fmt.Sprintf("m-%03d", i)
	}
	enqueue(t, s, ids...)

	var (
		mu   sync.Mutex
		seen = map[string]int{}
		wg   sync.WaitGroup
	)
	publish := func(_ context.Context, r outbox.Record) error {
		time.Sleep(5 * time.Millisecond) // widen the race window
		mu.Lock()
		seen[r.ID]++
		mu.Unlock()
		return nil
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n, err := s.Dispatch(context.Background(), 10, publish)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()

	if len(seen) != total {
		t.Fatalf("published %d distinct rows, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("row %s published %d times", id, n)
		}
	}
}

func TestBacklogCountsUnpublishedMessagesAndTheAgeOfTheOldest(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()

	if n, age, err := s.Backlog(ctx); err != nil || n != 0 || age != 0 {
		t.Fatalf("empty outbox: %d, %v, %v; want 0, 0", n, age, err)
	}

	enqueue(t, s, "a", "b", "c")
	time.Sleep(50 * time.Millisecond)
	n, age, err := s.Backlog(ctx)
	if err != nil || n != 3 || age < 50*time.Millisecond || age > time.Minute {
		t.Fatalf("after enqueue: %d, %v, %v; want 3 messages, the oldest at least 50ms old", n, age, err)
	}

	if _, err := s.Dispatch(ctx, 2, func(context.Context, outbox.Record) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := s.Backlog(ctx); n != 1 {
		t.Fatalf("after publishing two: %d pending, want 1", n)
	}

	if _, err := s.Dispatch(ctx, 10, func(context.Context, outbox.Record) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if n, age, _ := s.Backlog(ctx); n != 0 || age != 0 {
		t.Fatalf("after publishing everything: %d, %v; want 0, 0", n, age)
	}
}

func TestOnceRunsFunctionOnlyOnce(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()

	var calls int
	fn := func(context.Context) error { calls++; return nil }
	for range 3 {
		if err := s.Once(ctx, "inventory", "msg-1", fn); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Once(ctx, "inventory", "msg-2", fn); err != nil {
		t.Fatal(err)
	}
	if err := s.Once(ctx, "payment", "msg-1", fn); err != nil { // different consumer
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("fn ran %d times, want 3", calls)
	}
}

func TestOnceFailureAllowsRetry(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()

	boom := errors.New("boom")
	if err := s.Once(ctx, "inventory", "msg-1", func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}

	var calls int
	if err := s.Once(ctx, "inventory", "msg-1", func(context.Context) error { calls++; return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("retry ran fn %d times, want 1 (a failed attempt must not mark the message processed)", calls)
	}
}

func TestOnceRejectsMissingMessageID(t *testing.T) {
	s, _ := newStore(t)
	err := s.Once(context.Background(), "inventory", "", func(context.Context) error { return nil })
	if !errors.Is(err, messaging.ErrMissingID) {
		t.Fatalf("err = %v, want ErrMissingID", err)
	}
}
