package reliability

import (
	"fmt"
	"sync"
	"time"
)

// State is a circuit breaker position.
type State int

// Breaker states, following the spec diagram: CLOSED opens when the failure
// rate trips, OPEN cools down into HALF_OPEN, and the probe outcome decides
// between reopening and closing.
const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

// String implements fmt.Stringer with the spec's spelling of each state.
func (s State) String() string {
	switch s {
	case StateClosed:
		return "CLOSED"
	case StateOpen:
		return "OPEN"
	case StateHalfOpen:
		return "HALF_OPEN"
	default:
		return "UNKNOWN"
	}
}

// BreakerConfig tunes one breaker. The engine fills it from per-runner
// config with engine defaults.
type BreakerConfig struct {
	// FailureThreshold opens the breaker when the failure rate over Window
	// meets or exceeds it (0.5 means half the calls failed).
	FailureThreshold float64
	// Window is the rolling period outcomes are counted over.
	Window time.Duration
	// MinRequests is the smallest sample count that can trip the breaker, so
	// one failure out of two calls does not shed a healthy service.
	MinRequests int
	// Cooldown is how long the breaker stays OPEN before admitting a single
	// HALF_OPEN probe.
	Cooldown time.Duration
}

// sample is one recorded call outcome inside the rolling window.
type sample struct {
	at      time.Time
	success bool
}

// Breaker guards a single runner so one failing service does not drag down
// every workflow using it. Safe for concurrent use.
type Breaker struct {
	key      string
	config   BreakerConfig
	callback func(key string, to State)

	mu       sync.Mutex
	state    State
	samples  []sample
	openedAt time.Time
	probing  bool
}

// NewBreaker builds a CLOSED breaker for one runner key.
func NewBreaker(key string, config BreakerConfig) *Breaker {
	return &Breaker{key: key, config: config}
}

// Allow reports whether a call may proceed. While the breaker is OPEN it
// fails fast with ErrCircuitOpen until Cooldown elapses, after which exactly
// one caller is admitted as the HALF_OPEN probe; everyone else keeps failing
// fast until Record settles the probe.
func (b *Breaker) Allow() error {
	allowed, transitioned := b.admit(time.Now())

	if transitioned != nil {
		b.notify(*transitioned)
	}

	if !allowed {
		return fmt.Errorf("%w: runner %q", ErrCircuitOpen, b.key)
	}

	return nil
}

// OnTransition registers a callback fired after every state change, for
// metrics and logging. Register it before the breaker sees traffic; the
// field itself is not synchronized.
func (b *Breaker) OnTransition(callback func(key string, to State)) {
	b.callback = callback
}

// Record feeds one call outcome into the breaker.
func (b *Breaker) Record(success bool) {
	transitioned := b.applyOutcome(success, time.Now())

	if transitioned != nil {
		b.notify(*transitioned)
	}
}

// Reset forces the breaker CLOSED and clears its rolling window: the
// operator override behind the admin API, for a breaker stuck open after
// an outage the operator knows has ended. A breaker already CLOSED only
// drops its samples; the transition callback fires only when the position
// actually changed.
func (b *Breaker) Reset() {
	transitioned := b.applyReset()

	if transitioned != nil {
		b.notify(*transitioned)
	}
}

// State reports the breaker's current position.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.state
}

// admit applies the state machine for one incoming call and reports whether
// it may proceed, plus any transition that happened so the caller can notify
// outside the lock.
func (b *Breaker) admit(now time.Time) (bool, *State) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateOpen:
		if now.Sub(b.openedAt) < b.config.Cooldown {
			return false, nil
		}

		b.state = StateHalfOpen
		b.probing = true
		halfOpen := StateHalfOpen

		return true, &halfOpen
	case StateHalfOpen:
		if b.probing {
			return false, nil
		}

		b.probing = true

		return true, nil
	case StateClosed:
		return true, nil
	default:
		return true, nil
	}
}

// applyOutcome folds one outcome into the state machine under the lock,
// returning any transition for out-of-lock notification.
func (b *Breaker) applyOutcome(success bool, now time.Time) *State {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		b.samples = append(b.samples, sample{at: now, success: success})
		b.prune(now)

		if b.shouldOpen() {
			b.state = StateOpen
			b.openedAt = now
			opened := StateOpen

			return &opened
		}

		return nil
	case StateHalfOpen:
		b.probing = false

		if success {
			b.state = StateClosed
			b.samples = b.samples[:0]
			closed := StateClosed

			return &closed
		}

		b.state = StateOpen
		b.openedAt = now
		reopened := StateOpen

		return &reopened
	case StateOpen:
		// A straggler that started before the breaker opened; its outcome
		// says nothing about recovery, so it is dropped.
		return nil
	default:
		return nil
	}
}

// applyReset clears the state machine under the lock, returning any
// transition for out-of-lock notification.
func (b *Breaker) applyReset() *State {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.samples = b.samples[:0]
	b.probing = false

	if b.state == StateClosed {
		return nil
	}

	b.state = StateClosed
	closed := StateClosed

	return &closed
}

// prune drops samples that fell out of the rolling window.
func (b *Breaker) prune(now time.Time) {
	cutoff := now.Add(-b.config.Window)

	kept := b.samples[:0]
	for _, item := range b.samples {
		if item.at.After(cutoff) {
			kept = append(kept, item)
		}
	}

	b.samples = kept
}

// shouldOpen checks the trip condition: at least MinRequests samples in the
// window with a failure rate at or above the threshold.
func (b *Breaker) shouldOpen() bool {
	if len(b.samples) < b.config.MinRequests {
		return false
	}

	var failures int

	for _, item := range b.samples {
		if !item.success {
			failures++
		}
	}

	return float64(failures)/float64(len(b.samples)) >= b.config.FailureThreshold
}

// notify fires the transition callback, when one is registered.
func (b *Breaker) notify(to State) {
	if b.callback != nil {
		b.callback(b.key, to)
	}
}

// BreakerSet keys one Breaker per runner name, creating breakers lazily with
// a shared config. Safe for concurrent use.
type BreakerSet struct {
	config   BreakerConfig
	callback func(key string, to State)

	mu       sync.Mutex
	breakers map[string]*Breaker
}

// NewBreakerSet builds an empty set sharing config across every key.
func NewBreakerSet(config BreakerConfig) *BreakerSet {
	return &BreakerSet{config: config, breakers: make(map[string]*Breaker)}
}

// Get returns the breaker for key, creating a CLOSED one on first use.
func (s *BreakerSet) Get(key string) *Breaker {
	s.mu.Lock()
	defer s.mu.Unlock()

	breaker, exists := s.breakers[key]
	if exists {
		return breaker
	}

	breaker = NewBreaker(key, s.config)
	if s.callback != nil {
		breaker.OnTransition(s.callback)
	}

	s.breakers[key] = breaker

	return breaker
}

// OnTransition registers a callback applied to every breaker the set creates
// from then on. Register it before handing the set to the engine.
func (s *BreakerSet) OnTransition(callback func(key string, to State)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.callback = callback
}

// Reset forces one breaker CLOSED, reporting whether that key was ever
// created. Unknown keys stay uncreated, so a typo cannot mint a breaker
// and pretend the reset worked.
func (s *BreakerSet) Reset(key string) bool {
	s.mu.Lock()
	breaker, exists := s.breakers[key]
	s.mu.Unlock()

	if !exists {
		return false
	}

	breaker.Reset()

	return true
}

// States snapshots every created breaker's position by key. Breakers exist
// only once their runner has been called, so a quiet runner is absent
// rather than reported CLOSED.
func (s *BreakerSet) States() map[string]State {
	s.mu.Lock()
	defer s.mu.Unlock()

	states := make(map[string]State, len(s.breakers))
	for key, breaker := range s.breakers {
		states[key] = breaker.State()
	}

	return states
}
