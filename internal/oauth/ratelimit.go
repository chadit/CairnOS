package oauth

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// loginRateDefaults bound the unauthenticated /v1/oauth/login route (spec L1).
// The route mints a state-store entry per call, so an unbounded caller could
// both exhaust the process and flood the pending-state map; the per-IP bucket
// throttles a single source. One login per second on average, with a
// full-minute burst, is generous for a human and tight against a flood.
const (
	defaultLoginRatePerMinute = 60

	// bucketIdleTTL prunes an IP's bucket after this much inactivity so the map
	// stays bounded by the active-source count rather than every IP that ever
	// hit the route.
	bucketIdleTTL = 10 * time.Minute
)

// loginBucket is one source IP's refill state: tokens accrue at ratePerMinute
// up to a full-minute burst, and a login spends one.
type loginBucket struct {
	lastSeen time.Time
	tokens   float64
}

// loginLimiter rate-limits the login route per source IP with a token bucket,
// and prunes idle buckets so the map cannot grow without bound. A nil limiter
// admits every call, the auth-style fail-open for an unconfigured guard. Safe
// for concurrent use.
type loginLimiter struct {
	now           func() time.Time
	buckets       map[string]*loginBucket
	ratePerMinute float64
	mu            sync.Mutex
}

// newLoginLimiter builds a limiter at the given per-minute rate, defaulting the
// rate and clock when unset.
func newLoginLimiter(ratePerMinute int, now func() time.Time) *loginLimiter {
	if ratePerMinute <= 0 {
		ratePerMinute = defaultLoginRatePerMinute
	}

	if now == nil {
		now = time.Now
	}

	return &loginLimiter{
		ratePerMinute: float64(ratePerMinute),
		now:           now,
		buckets:       map[string]*loginBucket{},
	}
}

// allow charges one token from the source IP's bucket, refilling by elapsed
// time first and pruning idle buckets, reporting whether the call may proceed.
func (l *loginLimiter) allow(source string) bool {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.pruneLocked(now)

	bucket, ok := l.buckets[source]
	if !ok {
		// A new source starts with a full burst minus the token this call
		// spends, so its first login always passes.
		l.buckets[source] = &loginBucket{tokens: l.ratePerMinute - 1, lastSeen: now}

		return true
	}

	refill := now.Sub(bucket.lastSeen).Minutes() * l.ratePerMinute
	bucket.tokens = min(bucket.tokens+refill, l.ratePerMinute)
	bucket.lastSeen = now

	if bucket.tokens < 1 {
		return false
	}

	bucket.tokens--

	return true
}

// pruneLocked drops buckets untouched past bucketIdleTTL. The caller holds mu.
func (l *loginLimiter) pruneLocked(now time.Time) {
	for source, bucket := range l.buckets {
		if now.Sub(bucket.lastSeen) > bucketIdleTTL {
			delete(l.buckets, source)
		}
	}
}

// clientIP extracts the source IP for rate limiting. RemoteAddr is host:port;
// the host alone keys the bucket so a source cannot dodge the limit by rotating
// ports. A parse failure falls back to the raw RemoteAddr, which still keys one
// bucket per source.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}
