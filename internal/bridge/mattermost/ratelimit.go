package mattermost

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Bucket defaults mirror the Mattermost server's own rate limiter
// (server/public/model/config.go: PerSec 10, MaxBurst 100), so a client running
// on the defaults never outruns a server running on its defaults.
const (
	defaultRatePerSecond = 10.0
	defaultBurst         = 100.0
)

// BucketConfig tunes a bucket. Zero or negative rate and burst take the
// defaults above, so a partially filled config still throttles.
type BucketConfig struct {
	// Now injects the clock for tests; nil means time.Now.
	Now func() time.Time

	// Sleep injects the wait for tests; nil means a context-aware timer.
	Sleep func(ctx context.Context, wait time.Duration) error

	RatePerSecond float64
	Burst         float64
}

// Bucket is a token bucket shared by every identity pointed at one server.
// Mattermost keys its own bucket by source IP, so identities in one process
// share the server's budget whether or not they share a limiter; one bucket per
// server URL is the only arrangement that matches (design D5). Safe for
// concurrent use.
type Bucket struct {
	// last, parked and tokens are guarded by mu; the rest is set once at
	// construction. The field order is the one the alignment check wants, not
	// a grouping by purpose.
	last time.Time

	// parked holds the server's own reset time after a 429, so every waiter
	// holds off until then instead of rediscovering the limit one at a time.
	parked time.Time

	now   func() time.Time
	sleep func(ctx context.Context, wait time.Duration) error

	rate   float64
	burst  float64
	tokens float64

	mu sync.Mutex
}

// NewBucket builds a bucket that starts full, so a freshly connected identity
// can burst through its startup calls without waiting.
func NewBucket(cfg BucketConfig) *Bucket {
	rate := cfg.RatePerSecond
	if rate <= 0 {
		rate = defaultRatePerSecond
	}

	burst := cfg.Burst
	if burst <= 0 {
		burst = defaultBurst
	}

	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	sleep := cfg.Sleep
	if sleep == nil {
		sleep = sleepContext
	}

	return &Bucket{now: now, sleep: sleep, rate: rate, burst: burst, tokens: burst, last: now()}
}

// Wait blocks until one token is available, or until ctx ends. A nil bucket
// admits every call so a client built without a limiter still works.
func (b *Bucket) Wait(ctx context.Context) error {
	if b == nil {
		return nil
	}

	for {
		delay, ok := b.reserve()
		if ok {
			return nil
		}

		if err := b.sleep(ctx, delay); err != nil {
			return err
		}
	}
}

// ParkFor holds every waiter for wait, which a 429 sources from the server's
// own X-Ratelimit-Reset header. A shorter park than the one already in force is
// ignored, so a stale 429 cannot cut a longer wait short.
func (b *Bucket) ParkFor(wait time.Duration) {
	if b == nil || wait <= 0 {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if until := b.now().Add(wait); until.After(b.parked) {
		b.parked = until
	}
}

// reserve spends one token, or reports how long the caller should wait before
// asking again.
func (b *Bucket) reserve() (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()

	if wait := b.parked.Sub(now); wait > 0 {
		return wait, false
	}

	b.tokens = min(b.tokens+now.Sub(b.last).Seconds()*b.rate, b.burst)
	b.last = now

	if b.tokens < 1 {
		return time.Duration((1 - b.tokens) / b.rate * float64(time.Second)), false
	}

	b.tokens--

	return 0, true
}

// sleepContext waits out wait, or returns early when ctx ends.
func sleepContext(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("mattermost rate limiter: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}
