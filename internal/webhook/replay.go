package webhook

import (
	"sync"
	"time"
)

// keySep joins a registration ID to a nonce or delivery id for the replay-cache
// key. A NUL byte cannot appear in a ULID registration ID, so it separates the
// two fields without a collision even when a nonce contains arbitrary bytes.
const keySep = "\x00"

// replayCache rejects repeated native nonces and GitHub delivery ids per
// registration, the freshness half of HMAC verification.
// HMAC over the body proves origin but not freshness: a captured signed request
// replays forever unless the receiver remembers what it accepted. Entries
// expire after the retention window so both maps stay bounded. Safe for
// concurrent use.
type replayCache struct {
	now        func() time.Time
	nonces     map[string]time.Time
	deliveries map[string]time.Time
	ttl        time.Duration
	mu         sync.Mutex
}

// newReplayCache builds a cache whose entries live for ttl, defaulting the clock
// when unset.
func newReplayCache(ttl time.Duration, now func() time.Time) *replayCache {
	if now == nil {
		now = time.Now
	}

	return &replayCache{
		ttl:        ttl,
		now:        now,
		nonces:     map[string]time.Time{},
		deliveries: map[string]time.Time{},
	}
}

// admitNonce records (registrationID, nonce) and reports whether it is fresh:
// true the first time inside the window, false for a repeat. Only a validly
// signed, in-window request should reach here, so a flood of bad signatures
// cannot pollute the cache.
func (c *replayCache) admitNonce(registrationID, nonce string) bool {
	return c.admit(c.nonces, registrationID+keySep+nonce)
}

// admitDelivery records (registrationID, deliveryID) and reports whether it is
// fresh: true the first time inside the window, false for a repeat. The GitHub
// header form carries no timestamp, so the delivery id is the only freshness
// signal it offers.
func (c *replayCache) admitDelivery(registrationID, deliveryID string) bool {
	return c.admit(c.deliveries, registrationID+keySep+deliveryID)
}

// admit prunes expired entries, then records key with a fresh expiry when it is
// new, reporting whether it was new. The caller must not have seen key inside
// the window for a true result.
func (c *replayCache) admit(entries map[string]time.Time, key string) bool {
	now := c.now()

	c.mu.Lock()
	defer c.mu.Unlock()

	pruneExpired(entries, now)

	if _, seen := entries[key]; seen {
		return false
	}

	entries[key] = now.Add(c.ttl)

	return true
}

// pruneExpired drops entries whose retention window has passed. The caller holds
// the cache mutex, so this mutates the shared map safely.
func pruneExpired(entries map[string]time.Time, now time.Time) {
	for key, expiry := range entries {
		if now.After(expiry) {
			delete(entries, key)
		}
	}
}
