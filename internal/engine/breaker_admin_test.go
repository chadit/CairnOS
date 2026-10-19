package engine_test

import (
	"sync"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/reliability"
	"github.com/chadit/CairnOS/internal/store"
)

// transitionLog records breaker transitions race-safely: the engine fires
// the callback from worker goroutines.
type transitionLog struct {
	entries []string
	mu      sync.Mutex
}

func (l *transitionLog) add(name string, to reliability.State) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.entries = append(l.entries, name+"->"+to.String())
}

func (l *transitionLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]string(nil), l.entries...)
}

// TestBreakerExposure proves the admin surface end to end: a failing run
// trips the mock runner's breaker open, the engine's breaker set reports it,
// Reset forces it closed, an unknown runner reports false, and the
// transition hook saw both moves.
func TestBreakerExposure(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Breaker = reliability.BreakerConfig{
		FailureThreshold: 0.5,
		Window:           time.Minute,
		MinRequests:      1,
		Cooldown:         time.Minute,
	}

	eng, _ := newTestEngine(t, cfg, nil)

	log := &transitionLog{}
	eng.Breakers().OnTransition(log.add)

	run := mustRun(t, eng, parseFlow(t, breakerTripTOML), nil)
	wantRunStatus(t, run, store.RunFailed)

	states := eng.Breakers().States()
	if states["mock"] != reliability.StateOpen {
		t.Fatalf("breaker state = %v, want OPEN (states: %v)", states["mock"], states)
	}

	if !eng.Breakers().Reset("mock") {
		t.Fatal("Reset(mock) = false, want true for an existing breaker")
	}

	if state := eng.Breakers().States()["mock"]; state != reliability.StateClosed {
		t.Fatalf("breaker state after reset = %v, want CLOSED", state)
	}

	if eng.Breakers().Reset("ghost") {
		t.Fatal("Reset(ghost) = true, want false for a runner with no breaker")
	}

	want := []string{"mock->OPEN", "mock->CLOSED"}

	got := log.snapshot()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("transitions = %v, want %v", got, want)
	}
}
