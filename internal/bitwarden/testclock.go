package bitwarden

import (
	"sync"
	"time"
)

// This file is not a _test.go file so internal/cli's tests can reach this clock
// as well. Both packages start the same fake bw, and a second copy of the clock
// in each of them is the duplicate fixture the test rules warn about.

// SteppingClock spends the readiness budget in poll counts rather than in
// seconds a loaded host can lose. Each wait still takes the real interval, so
// probing stays paced: a clock that fired at once would spend the whole budget
// before any child could bind.
type SteppingClock struct {
	mu      sync.Mutex
	elapsed time.Duration
	step    time.Duration
}

// NewSteppingClock builds a clock whose readiness budget lasts polls polls. One
// poll makes a wait give up on the first miss; a large count gives a loaded host
// room to start a child, and still ends the wait rather than hanging on a daemon
// that answers neither way.
func NewSteppingClock(polls int) *SteppingClock {
	if polls < 1 {
		polls = 1
	}

	return &SteppingClock{step: readyTimeout / time.Duration(polls)}
}

// Now answers with the total the polls so far have advanced.
func (c *SteppingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return time.Time{}.Add(c.elapsed)
}

// After spends one poll's slice of the budget and waits the real interval.
func (c *SteppingClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.elapsed += c.step
	c.mu.Unlock()

	return time.After(d)
}
