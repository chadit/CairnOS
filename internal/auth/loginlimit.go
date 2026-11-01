package auth

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

// Login DoS defaults (M1). A small derivation pool bounds how many
// unauthenticated argon2id derivations run at once, since each costs ~64 MiB;
// the per-peer bucket throttles a single source so it cannot monopolize even
// the bounded pool.
const (
	defaultMaxConcurrentLogins = 4
	defaultLoginRatePerMinute  = 60
)

// bucketIdleTTL prunes a peer's bucket after this much inactivity so the map
// does not grow with every source IP that ever logged in.
const bucketIdleTTL = 10 * time.Minute

// LoginLimiter caps unauthenticated login cost two ways: a bounded semaphore
// over the whole process limits concurrent credential derivations, and a
// per-peer token bucket rate-limits any single source. Both guard only the
// Login RPC; a refused call answers ErrTooManyLogins rather than queuing
// behind the argon2id work (spec M1). Safe for concurrent use.
type LoginLimiter struct {
	sem           *semaphore.Weighted
	now           func() time.Time
	buckets       map[string]*tokenBucket
	ratePerMinute int
	mu            sync.Mutex
}

// tokenBucket is one peer's refill state: tokens accrue at ratePerMinute up to
// a full-minute burst, and a login spends one.
type tokenBucket struct {
	lastSeen time.Time
	tokens   float64
}

// LoginLimitConfig tunes the limiter; zero or negative values fall back to the
// M1 defaults so a partially filled config still protects the login path.
type LoginLimitConfig struct {
	// Now injects the clock for tests; nil means time.Now.
	Now func() time.Time

	// MaxConcurrent bounds simultaneous credential derivations.
	MaxConcurrent int

	// RatePerMinute bounds one peer's logins per minute (also its burst).
	RatePerMinute int
}

// NewLoginLimiter builds a limiter from the config, applying the M1 defaults
// for any unset field.
func NewLoginLimiter(cfg LoginLimitConfig) *LoginLimiter {
	maxConcurrent := cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMaxConcurrentLogins
	}

	rate := cfg.RatePerMinute
	if rate <= 0 {
		rate = defaultLoginRatePerMinute
	}

	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	return &LoginLimiter{
		sem:           semaphore.NewWeighted(int64(maxConcurrent)),
		ratePerMinute: rate,
		now:           now,
		buckets:       make(map[string]*tokenBucket),
	}
}

// Acquire admits one login from peer, returning a release to call when the
// derivation finishes. It charges the peer's bucket first (cheap, no slot
// held) and then tries the derivation pool without blocking; either refusal is
// ErrTooManyLogins. A nil limiter admits every call so auth without the M1
// guard still works.
func (l *LoginLimiter) Acquire(ctx context.Context, peer string) (func(), error) {
	if l == nil {
		return func() {}, nil
	}

	if !l.allowPeer(peer) {
		return nil, fmt.Errorf("%w: peer %s exceeded the login rate", ErrTooManyLogins, peer)
	}

	if !l.sem.TryAcquire(1) {
		return nil, fmt.Errorf("%w: credential derivation pool is full", ErrTooManyLogins)
	}

	_ = ctx

	return func() { l.sem.Release(1) }, nil
}

// allowPeer charges one token from the peer's bucket, refilling by elapsed
// time first. It also opportunistically prunes idle buckets so the map stays
// bounded by the active-peer count.
func (l *LoginLimiter) allowPeer(peer string) bool {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.pruneLocked(now)

	bucket, ok := l.buckets[peer]
	if !ok {
		// A new peer starts with a full burst minus the token this call
		// spends, so its first login always passes.
		l.buckets[peer] = &tokenBucket{tokens: float64(l.ratePerMinute) - 1, lastSeen: now}

		return true
	}

	refill := now.Sub(bucket.lastSeen).Minutes() * float64(l.ratePerMinute)
	bucket.tokens = min(bucket.tokens+refill, float64(l.ratePerMinute))
	bucket.lastSeen = now

	if bucket.tokens < 1 {
		return false
	}

	bucket.tokens--

	return true
}

// pruneLocked drops buckets untouched past bucketIdleTTL. The caller holds mu.
func (l *LoginLimiter) pruneLocked(now time.Time) {
	for peer, bucket := range l.buckets {
		if now.Sub(bucket.lastSeen) > bucketIdleTTL {
			delete(l.buckets, peer)
		}
	}
}
