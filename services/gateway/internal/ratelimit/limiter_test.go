package ratelimit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

var start = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestBurstThenReject(t *testing.T) {
	c := &clock{start}
	l := New(1, 3, Options{Now: c.now}) // 1 request/second, burst of 3

	for i := range 3 {
		if ok, _ := l.Allow("alice"); !ok {
			t.Fatalf("request %d within the burst was rejected", i+1)
		}
	}
	ok, retry := l.Allow("alice")
	if ok {
		t.Fatal("the fourth request must be rejected")
	}
	if retry <= 0 || retry > time.Second {
		t.Errorf("retryAfter = %v, want a positive wait of at most one token interval", retry)
	}
}

func TestTokensRefillOverTime(t *testing.T) {
	c := &clock{start}
	l := New(2, 2, Options{Now: c.now}) // 2 per second
	l.Allow("a")
	l.Allow("a")
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("bucket should be empty")
	}

	c.advance(500 * time.Millisecond) // one token back
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("a token should have refilled after 500ms at 2/s")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("only one token should have refilled")
	}

	c.advance(time.Hour) // refill is capped at the burst
	for range 2 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatal("the full burst should be available again")
		}
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("refill must not exceed the burst")
	}
}

func TestRejectedRequestsDoNotConsumeTokens(t *testing.T) {
	c := &clock{start}
	l := New(1, 1, Options{Now: c.now})
	l.Allow("a")

	// Hammering while empty must not push the next allowed time further away.
	for range 100 {
		l.Allow("a")
	}
	c.advance(time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("after one refill interval a request must be allowed, however many were rejected before")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	c := &clock{start}
	l := New(1, 1, Options{Now: c.now})

	if ok, _ := l.Allow("alice"); !ok {
		t.Fatal("alice's first request")
	}
	if ok, _ := l.Allow("alice"); ok {
		t.Fatal("alice is over her limit")
	}
	if ok, _ := l.Allow("bob"); !ok {
		t.Fatal("alice being limited must not affect bob")
	}
}

func TestCleanupForgetsIdleKeys(t *testing.T) {
	c := &clock{start}
	l := New(1, 1, Options{Now: c.now, IdleTTL: time.Minute})
	l.Allow("old")
	c.advance(2 * time.Minute)
	l.Allow("recent")

	l.Cleanup()
	if l.Len() != 1 {
		t.Fatalf("%d keys tracked, want only the recent one", l.Len())
	}
	// A forgotten key simply starts with a full bucket again.
	if ok, _ := l.Allow("old"); !ok {
		t.Fatal("a forgotten key must be allowed again")
	}
}

func TestRunCleansUpUntilCancelled(t *testing.T) {
	l := New(1, 1, Options{IdleTTL: time.Millisecond})
	l.Allow("a")
	time.Sleep(5 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx, 5*time.Millisecond) }()

	deadline := time.Now().Add(2 * time.Second)
	for l.Len() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if l.Len() != 0 {
		t.Error("the idle key was never cleaned up")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v, want context.Canceled", err)
	}
}

func TestConcurrentUseIsSafeAndNeverExceedsTheBurst(t *testing.T) {
	c := &clock{start} // time stands still, so exactly `burst` requests may pass
	l := New(1, 10, Options{Now: c.now})

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		allowed int
	)
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.Allow("shared"); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 10 {
		t.Fatalf("%d requests allowed, want exactly the burst of 10", allowed)
	}
}
