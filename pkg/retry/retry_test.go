package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

var fast = Options{Initial: time.Millisecond, Max: 5 * time.Millisecond}

func TestSucceedsOnTheFirstTry(t *testing.T) {
	calls := 0
	if err := DoWith(context.Background(), time.Second, fast, func(context.Context) error { calls++; return nil }); err != nil || calls != 1 {
		t.Fatalf("calls = %d, err = %v", calls, err)
	}
}

func TestRetriesUntilTheOperationSucceeds(t *testing.T) {
	calls := 0
	err := DoWith(context.Background(), 5*time.Second, fast, func(context.Context) error {
		calls++
		if calls < 4 {
			return errors.New("connection refused")
		}
		return nil
	})
	if err != nil || calls != 4 {
		t.Fatalf("calls = %d, err = %v; want success on the 4th attempt", calls, err)
	}
}

func TestGivesUpAfterMaxWaitAndReturnsTheLastError(t *testing.T) {
	boom := errors.New("still down")
	calls := 0
	start := time.Now()
	err := DoWith(context.Background(), 60*time.Millisecond, fast, func(context.Context) error { calls++; return boom })

	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the last error", err)
	}
	if calls < 3 {
		t.Errorf("only %d attempts in 60ms; it should keep trying", calls)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v; it must stop at maxWait", elapsed)
	}
}

func TestAPermanentErrorStopsImmediately(t *testing.T) {
	bad := errors.New("invalid connection string")
	calls := 0
	err := DoWith(context.Background(), time.Minute, fast, func(context.Context) error { calls++; return Permanent(bad) })

	if calls != 1 {
		t.Fatalf("calls = %d, want 1: retrying cannot fix a permanent error", calls)
	}
	if !errors.Is(err, bad) {
		t.Fatalf("err = %v, want the original error", err)
	}
	if Permanent(nil) != nil {
		t.Error("Permanent(nil) must be nil")
	}
}

func TestCancellationStopsTheWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	done := make(chan error, 1)
	go func() {
		done <- DoWith(ctx, time.Minute, Options{Initial: 50 * time.Millisecond, Max: 50 * time.Millisecond}, func(context.Context) error {
			calls++
			return errors.New("down")
		})
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want it to mention the cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Do kept waiting after the context was cancelled")
	}
}

func TestBackoffGrowsButIsCapped(t *testing.T) {
	var stamps []time.Time
	_ = DoWith(context.Background(), 150*time.Millisecond, Options{Initial: 5 * time.Millisecond, Max: 20 * time.Millisecond}, func(context.Context) error {
		stamps = append(stamps, time.Now())
		return errors.New("down")
	})
	if len(stamps) < 4 {
		t.Fatalf("%d attempts", len(stamps))
	}
	first, later := stamps[1].Sub(stamps[0]), stamps[len(stamps)-1].Sub(stamps[len(stamps)-2])
	if later < first {
		t.Errorf("gaps %v then %v: the wait should grow", first, later)
	}
	if later > 80*time.Millisecond {
		t.Errorf("last gap %v exceeds the cap by far", later)
	}
}

func TestDefaultsApply(t *testing.T) {
	// Do uses the default backoff; a single failure followed by success must still work.
	calls := 0
	err := Do(context.Background(), 5*time.Second, func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("not yet")
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("calls = %d, err = %v", calls, err)
	}
}
