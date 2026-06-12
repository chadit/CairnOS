package oauth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"
)

// stateBytes sizes the random state token; 32 bytes of CSPRNG output is well
// past guessing range, the same width the access-key secret uses.
const stateBytes = 32

// stateEntry is one pending login: the PKCE verifier the callback replays to
// the IdP, and the expiry past which the state is rejected. A pending login
// that never returns is swept when its entry expires.
type stateEntry struct {
	verifier  string
	expiresAt time.Time
}

// stateStore holds pending login states server-side, keyed by the random state
// value. A callback's state must match a live entry, which both binds the
// callback to a login this server started (CSRF defense) and carries the PKCE
// verifier the exchange needs. Single-use: a matched state is consumed so a
// replayed callback fails. Safe for concurrent use.
type stateStore struct {
	now func() time.Time
	ttl time.Duration

	mu      sync.Mutex
	pending map[string]stateEntry
}

// newStateStore builds a store with the given TTL and clock. A nil clock uses
// time.Now; a non-positive TTL falls back to the default.
func newStateStore(ttl time.Duration, now func() time.Time) *stateStore {
	if now == nil {
		now = time.Now
	}

	if ttl <= 0 {
		ttl = defaultStateTTL
	}

	return &stateStore{
		now:     now,
		ttl:     ttl,
		pending: map[string]stateEntry{},
	}
}

// defaultStateTTL bounds how long a started login may sit before its callback
// must return; ten minutes is long enough for a human to authenticate at the
// IdP and short enough that an abandoned login's state does not linger.
const defaultStateTTL = 10 * time.Minute

// issue mints a fresh state value, stores it with the PKCE verifier and an
// expiry, and returns the state to put in the authorization URL. It also
// sweeps expired entries so an abandoned-login leak cannot grow without bound.
func (s *stateStore) issue(verifier string) (string, error) {
	raw := make([]byte, stateBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate oauth state: %w", err)
	}

	state := base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.sweepLocked(now)
	s.pending[state] = stateEntry{verifier: verifier, expiresAt: now.Add(s.ttl)}

	return state, nil
}

// consume looks up and removes a state, returning its PKCE verifier. A missing,
// expired, or already-consumed state reports false, which the callback treats
// as ErrStateInvalid: the CSRF and replay defense. Consuming on match makes
// every state single-use.
func (s *stateStore) consume(state string) (string, bool) {
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, found := s.pending[state]
	if !found {
		return "", false
	}

	delete(s.pending, state)

	if now.After(entry.expiresAt) {
		return "", false
	}

	return entry.verifier, true
}

// sweepLocked drops expired entries. The caller holds the lock.
func (s *stateStore) sweepLocked(now time.Time) {
	for key, entry := range s.pending {
		if now.After(entry.expiresAt) {
			delete(s.pending, key)
		}
	}
}
