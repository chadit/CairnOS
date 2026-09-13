package mattermost_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/bridge/mattermost"
)

// A testClock is a clock the caller drives by hand. Sleep advances it, so a
// bucket spends its waits instantly and the caller can assert what it waited
// for.
type testClock struct {
	now   time.Time
	slept []time.Duration
	mu    sync.Mutex
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, time.September, 12, 9, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *testClock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)

	return nil
}

func (c *testClock) waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]time.Duration(nil), c.slept...)
}

func TestBucketSpendsBurstThenWaitsForRefill(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	bucket := mattermost.NewBucket(mattermost.BucketConfig{
		Now: clock.Now, Sleep: clock.Sleep, RatePerSecond: 10, Burst: 2,
	})

	for call := range 3 {
		if err := bucket.Wait(t.Context()); err != nil {
			t.Fatalf("Wait call %d: %v", call+1, err)
		}
	}

	waits := clock.waits()
	if len(waits) != 1 {
		t.Fatalf("waited %v, want exactly one wait after the burst was spent", waits)
	}

	// Ten tokens a second means the third call waits a tenth of a second.
	if waits[0] != 100*time.Millisecond {
		t.Fatalf("waited %v for a refill at 10/s, want 100ms", waits[0])
	}
}

func TestBucketParkForHoldsUntilTheServerReset(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	bucket := mattermost.NewBucket(mattermost.BucketConfig{
		Now: clock.Now, Sleep: clock.Sleep, RatePerSecond: 10, Burst: 100,
	})

	bucket.ParkFor(5 * time.Second)

	if err := bucket.Wait(t.Context()); err != nil {
		t.Fatalf("Wait after a park: %v", err)
	}

	waits := clock.waits()
	if len(waits) != 1 || waits[0] != 5*time.Second {
		t.Fatalf("waited %v, want one five-second park even with tokens in hand", waits)
	}
}

func TestBucketParkForKeepsTheLongerWait(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	bucket := mattermost.NewBucket(mattermost.BucketConfig{
		Now: clock.Now, Sleep: clock.Sleep, RatePerSecond: 10, Burst: 100,
	})

	bucket.ParkFor(10 * time.Second)
	bucket.ParkFor(time.Second)
	bucket.ParkFor(0)

	if err := bucket.Wait(t.Context()); err != nil {
		t.Fatalf("Wait after two parks: %v", err)
	}

	waits := clock.waits()
	if len(waits) != 1 || waits[0] != 10*time.Second {
		t.Fatalf("waited %v, want the longer park to stand", waits)
	}
}

func TestBucketWaitReturnsTheContextError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	bucket := mattermost.NewBucket(mattermost.BucketConfig{
		Now: time.Now,
		Sleep: func(ctx context.Context, _ time.Duration) error {
			cancel()

			return ctx.Err()
		},
		RatePerSecond: 1,
		Burst:         1,
	})

	if err := bucket.Wait(ctx); err != nil {
		t.Fatalf("the first Wait spends the only token: %v", err)
	}

	err := bucket.Wait(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait on a canceled context = %v, want context.Canceled", err)
	}
}

func TestNilBucketAdmitsEveryCall(t *testing.T) {
	t.Parallel()

	var bucket *mattermost.Bucket

	bucket.ParkFor(time.Hour)

	if err := bucket.Wait(t.Context()); err != nil {
		t.Fatalf("a nil bucket refused a call: %v", err)
	}
}
