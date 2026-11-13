package webhook

import (
	"sync"
	"time"
)

// This per-registration limiter is written fresh rather than shared with
// internal/oauth/ratelimit.go on purpose. The two
// limiters differ in shape, not just parameters: the login limiter keys on
// client IP, runs one global rate, and prunes idle buckets because its key
// space is every IP that ever hits the route. This one keys on registration ID,
// needs a per-key rate (a registration may override the config default), and
// needs no pruning at all because the key space is bounded by the registrations
// table. The only genuine overlap is the refill arithmetic in bucket.refill. A
// third token-bucket caller is the signal to extract a shared core.

const (
	// defaultRatePerMinute caps one registration's deliveries when neither the
	// registration override nor the config default sets a rate.
	defaultRatePerMinute = 60

	// defaultUnknownRatePerMinute caps unknown-token requests process-wide:
	// a generous global rate that sheds a flood of
	// unmatched tokens without allocating per token or per source.
	defaultUnknownRatePerMinute = 120
)

// bucket is one token bucket's refill state: tokens accrue at a per-minute rate
// up to a one-minute burst, and each admitted call spends one.
type bucket struct {
	last   time.Time
	tokens float64
}

// refill accrues tokens for the time elapsed since last at ratePerMinute,
// capped at a one-minute burst, and stamps last. A zero last marks a fresh
// bucket, which starts full so a registration's first delivery always passes.
func (b *bucket) refill(ratePerMinute float64, now time.Time) {
	if b.last.IsZero() {
		b.tokens = ratePerMinute
		b.last = now

		return
	}

	b.tokens = min(b.tokens+now.Sub(b.last).Minutes()*ratePerMinute, ratePerMinute)
	b.last = now
}

// spend takes one token when the bucket has one, reporting whether the call may
// proceed.
func (b *bucket) spend() bool {
	if b.tokens < 1 {
		return false
	}

	b.tokens--

	return true
}

// registrationLimiter caps each registration's delivery rate with its own token
// bucket. The rate is the registration's override when positive, else the
// configured default. A removed registration's bucket lingers until restart;
// its token no longer resolves, so nothing refills or reads it. Bounded,
// accepted. Safe for concurrent use.
type registrationLimiter struct {
	now         func() time.Time
	buckets     map[string]*bucket
	defaultRate float64
	mu          sync.Mutex
}

// newRegistrationLimiter builds a limiter at the given default per-minute rate,
// defaulting the rate and clock when unset.
func newRegistrationLimiter(ratePerMinute int, now func() time.Time) *registrationLimiter {
	if ratePerMinute <= 0 {
		ratePerMinute = defaultRatePerMinute
	}

	if now == nil {
		now = time.Now
	}

	return &registrationLimiter{
		defaultRate: float64(ratePerMinute),
		now:         now,
		buckets:     map[string]*bucket{},
	}
}

// allow charges one token from the registration's bucket at the effective rate
// (the override when positive, else the default), refilling by elapsed time
// first, and reports whether the delivery may proceed.
func (l *registrationLimiter) allow(registrationID string, override int) bool {
	rate := l.defaultRate
	if override > 0 {
		rate = float64(override)
	}

	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	current, ok := l.buckets[registrationID]
	if !ok {
		current = &bucket{}
		l.buckets[registrationID] = current
	}

	current.refill(rate, now)

	return current.spend()
}

// globalLimiter is one process-wide token bucket for unknown-token requests.
// It allocates nothing per token or per source, so a
// flood of unmatched tokens is throttled to a fixed rate without growing state.
// Safe for concurrent use.
type globalLimiter struct {
	now  func() time.Time
	b    bucket
	rate float64
	mu   sync.Mutex
}

// newGlobalLimiter builds the shared unknown-token bucket at the given
// per-minute rate, defaulting the rate and clock when unset.
func newGlobalLimiter(ratePerMinute int, now func() time.Time) *globalLimiter {
	if ratePerMinute <= 0 {
		ratePerMinute = defaultUnknownRatePerMinute
	}

	if now == nil {
		now = time.Now
	}

	return &globalLimiter{rate: float64(ratePerMinute), now: now}
}

// allow charges one token from the shared bucket, refilling first, and reports
// whether the unknown-token request may be answered (404) or must be shed (429).
func (g *globalLimiter) allow() bool {
	now := g.now()

	g.mu.Lock()
	defer g.mu.Unlock()

	g.b.refill(g.rate, now)

	return g.b.spend()
}
