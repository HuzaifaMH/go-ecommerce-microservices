// Package retry retries an operation with exponential backoff. Services use it
// at start-up so that a dependency which is a few seconds late (a database
// still initialising, a broker still booting) does not crash them.
package retry

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Options tune the backoff. Zero values pick the defaults.
type Options struct {
	// Initial is the first wait between attempts. Default 100ms.
	Initial time.Duration
	// Max caps the wait between attempts. Default 2s.
	Max time.Duration
}

type permanent struct{ err error }

func (p permanent) Error() string { return p.err.Error() }
func (p permanent) Unwrap() error { return p.err }

// Permanent marks err as one that retrying cannot fix (for example an invalid
// configuration); Do returns it immediately.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanent{err}
}

// Do calls fn until it succeeds, returns a Permanent error, ctx is cancelled,
// or maxWait has passed since the first attempt. Waits between attempts double
// from the initial wait up to the maximum. On giving up it returns the last
// error from fn.
func Do(ctx context.Context, maxWait time.Duration, fn func(ctx context.Context) error) error {
	return DoWith(ctx, maxWait, Options{}, fn)
}

// DoWith is Do with explicit backoff options.
func DoWith(ctx context.Context, maxWait time.Duration, o Options, fn func(ctx context.Context) error) error {
	if o.Initial <= 0 {
		o.Initial = 100 * time.Millisecond
	}
	if o.Max <= 0 {
		o.Max = 2 * time.Second
	}

	deadline := time.Now().Add(maxWait)
	wait := o.Initial
	for attempt := 1; ; attempt++ {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		var p permanent
		if errors.As(err, &p) {
			return p.err
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%w (stopped: %w)", err, ctx.Err())
		}
		if !time.Now().Add(wait).Before(deadline) {
			return fmt.Errorf("gave up after %d attempts over %s: %w", attempt, maxWait.Round(time.Millisecond), err)
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%w (stopped: %w)", err, ctx.Err())
		case <-timer.C:
		}
		wait = min(wait*2, o.Max)
	}
}
