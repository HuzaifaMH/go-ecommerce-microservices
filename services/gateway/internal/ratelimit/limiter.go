// Package ratelimit limits how fast each client may call the API, using a
// token bucket per key.
//
// State is kept in memory, so with several gateway instances each enforces
// the limit independently (the effective limit is rate x instances). A shared
// store such as Redis would make it global; that is a deliberate non-goal here.
package ratelimit

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type entry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// Limiter hands out one token bucket per key.
type Limiter struct {
	rate  rate.Limit
	burst int
	idle  time.Duration
	now   func() time.Time

	mu      sync.Mutex
	entries map[string]*entry
}

// Options tune a Limiter.
type Options struct {
	// IdleTTL is how long a key may stay unused before its bucket is
	// forgotten, which bounds memory. Default 10 minutes.
	IdleTTL time.Duration
	// Now is the clock. Defaults to time.Now.
	Now func() time.Time
}

// New returns a Limiter allowing rps requests per second per key, with bursts
// of up to burst requests.
func New(rps float64, burst int, o Options) *Limiter {
	if o.IdleTTL == 0 {
		o.IdleTTL = 10 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Limiter{rate: rate.Limit(rps), burst: burst, idle: o.IdleTTL, now: o.Now, entries: map[string]*entry{}}
}

// Allow takes one token for key. When none is available it returns false and
// how long the caller should wait before trying again.
func (l *Limiter) Allow(key string) (allowed bool, retryAfter time.Duration) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entries[key]
	if !ok {
		e = &entry{limiter: rate.NewLimiter(l.rate, l.burst)}
		l.entries[key] = e
	}
	e.lastSeen = now

	r := e.limiter.ReserveN(now, 1)
	if !r.OK() {
		return false, time.Second // the burst is smaller than one request; cannot happen with burst >= 1
	}
	if delay := r.DelayFrom(now); delay > 0 {
		r.CancelAt(now) // do not consume a token for a rejected request
		return false, delay
	}
	return true, 0
}

// Cleanup forgets keys that have been idle longer than the idle TTL.
func (l *Limiter) Cleanup() {
	cutoff := l.now().Add(-l.idle)

	l.mu.Lock()
	defer l.mu.Unlock()
	for k, e := range l.entries {
		if e.lastSeen.Before(cutoff) {
			delete(l.entries, k)
		}
	}
}

// Len returns the number of keys currently tracked.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// Run calls Cleanup every interval until ctx is cancelled. It is meant to be
// run as a runner.Task.
func (l *Limiter) Run(ctx context.Context, interval time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			l.Cleanup()
		}
	}
}
