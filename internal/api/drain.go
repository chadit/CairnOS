package api

import (
	"sync"
	"time"
)

// Drain is the process-lifecycle switch RequestRestart throws: once it is on,
// the serve wiring stops dispatching new scheduled fires and waits for the
// in-flight runs to finish before canceling its root context. It lives here
// rather than in the serve wiring because the RPC and the wiring both need it
// and the RPC cannot import the CLI.
type Drain struct {
	requested chan struct{}
	deadline  time.Time
	mu        sync.Mutex
	draining  bool
}

// NewDrain builds an idle Drain. Requested stays open until the first request.
func NewDrain() *Drain {
	return &Drain{requested: make(chan struct{})}
}

// Request starts the drain and reports the deadline it will give up at. A
// second request while already draining keeps the first deadline, so a retrying
// client cannot push the exit further out.
func (d *Drain) Request(maxWait time.Duration, now time.Time) time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.draining {
		return d.deadline
	}

	d.draining = true
	d.deadline = now.Add(maxWait)
	close(d.requested)

	return d.deadline
}

// Draining reports whether a restart has been asked for.
func (d *Drain) Draining() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.draining
}

// Deadline is when the drain gives up on the remaining runs, zero while idle.
func (d *Drain) Deadline() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.deadline
}

// Requested closes on the first request, so the watcher waits on a channel
// instead of polling the flag.
func (d *Drain) Requested() <-chan struct{} {
	return d.requested
}
