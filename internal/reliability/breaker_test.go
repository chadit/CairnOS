package reliability_test

import (
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/chadit/CairnOS/internal/reliability"
)

const breakerKey = "openai"

// testConfig builds a breaker that opens at a 50% failure rate over 4+
// samples, with a cooldown short enough to keep the suite fast.
func testConfig() reliability.BreakerConfig {
	return reliability.BreakerConfig{
		FailureThreshold: 0.5,
		Window:           time.Second,
		MinRequests:      4,
		Cooldown:         20 * time.Millisecond,
	}
}

// tripBreaker records the given number of failures.
func tripBreaker(breaker *reliability.Breaker, failures int) {
	for range failures {
		breaker.Record(false)
	}
}

func TestBreakerOpensAtThresholdAndFailsFast(t *testing.T) {
	t.Parallel()

	breaker := reliability.NewBreaker(breakerKey, testConfig())

	breaker.Record(true)
	breaker.Record(true)

	if state := breaker.State(); state != reliability.StateClosed {
		t.Fatalf("state = %v, want CLOSED before the threshold trips", state)
	}

	tripBreaker(breaker, 2) // 2 failures out of 4 samples hits the 0.5 threshold exactly

	if state := breaker.State(); state != reliability.StateOpen {
		t.Fatalf("state = %v, want OPEN", state)
	}

	err := breaker.Allow()
	if !errors.Is(err, reliability.ErrCircuitOpen) {
		t.Fatalf("allow while open = %v, want ErrCircuitOpen", err)
	}
}

func TestBreakerMinRequestsPreventsTrippingOnTinySamples(t *testing.T) {
	t.Parallel()

	breaker := reliability.NewBreaker(breakerKey, testConfig())

	tripBreaker(breaker, 3) // 100% failures, but still below MinRequests of 4

	if state := breaker.State(); state != reliability.StateClosed {
		t.Fatalf("state = %v, want CLOSED below MinRequests", state)
	}

	if err := breaker.Allow(); err != nil {
		t.Fatalf("allow = %v, want nil", err)
	}
}

func TestBreakerHalfOpenProbeSuccessCloses(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		config := testConfig()
		breaker := reliability.NewBreaker(breakerKey, config)

		tripBreaker(breaker, 4)
		time.Sleep(config.Cooldown + time.Millisecond)

		if err := breaker.Allow(); err != nil {
			t.Fatalf("probe allow = %v, want nil after cooldown", err)
		}

		if state := breaker.State(); state != reliability.StateHalfOpen {
			t.Fatalf("state = %v, want HALF_OPEN", state)
		}

		if err := breaker.Allow(); !errors.Is(err, reliability.ErrCircuitOpen) {
			t.Fatalf("second allow during probe = %v, want ErrCircuitOpen", err)
		}

		breaker.Record(true)

		if state := breaker.State(); state != reliability.StateClosed {
			t.Fatalf("state = %v, want CLOSED after probe success", state)
		}

		if err := breaker.Allow(); err != nil {
			t.Fatalf("allow after close = %v, want nil", err)
		}
	})
}

func TestBreakerHalfOpenProbeFailureReopens(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		config := testConfig()
		breaker := reliability.NewBreaker(breakerKey, config)

		tripBreaker(breaker, 4)
		time.Sleep(config.Cooldown + time.Millisecond)

		if err := breaker.Allow(); err != nil {
			t.Fatalf("probe allow = %v, want nil after cooldown", err)
		}

		breaker.Record(false)

		if state := breaker.State(); state != reliability.StateOpen {
			t.Fatalf("state = %v, want OPEN after probe failure", state)
		}

		if err := breaker.Allow(); !errors.Is(err, reliability.ErrCircuitOpen) {
			t.Fatalf("allow after reopen = %v, want ErrCircuitOpen", err)
		}
	})
}

func TestBreakerWindowExpiresOldFailures(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		config := testConfig()
		config.Window = 20 * time.Millisecond
		breaker := reliability.NewBreaker(breakerKey, config)

		tripBreaker(breaker, 3)
		time.Sleep(config.Window + time.Millisecond)

		breaker.Record(false) // stale failures fell out of the window: 1 sample is below MinRequests

		if state := breaker.State(); state != reliability.StateClosed {
			t.Fatalf("state = %v, want CLOSED once stale failures expire", state)
		}
	})
}

func TestBreakerTransitionCallbackSeesEveryStateChange(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		config := testConfig()
		breaker := reliability.NewBreaker(breakerKey, config)

		var transitions []reliability.State

		breaker.OnTransition(func(key string, to reliability.State) {
			if key != breakerKey {
				t.Errorf("callback key = %q, want %q", key, breakerKey)
			}

			transitions = append(transitions, to)
		})

		tripBreaker(breaker, 4)
		time.Sleep(config.Cooldown + time.Millisecond)

		if err := breaker.Allow(); err != nil {
			t.Fatalf("probe allow = %v", err)
		}

		breaker.Record(true)

		want := []reliability.State{reliability.StateOpen, reliability.StateHalfOpen, reliability.StateClosed}
		if !slices.Equal(transitions, want) {
			t.Fatalf("transitions = %v, want %v", transitions, want)
		}
	})
}

func TestBreakerSetKeysBreakersIndependently(t *testing.T) {
	t.Parallel()

	set := reliability.NewBreakerSet(testConfig())

	failing := set.Get("flaky_api")
	healthy := set.Get("steady_api")

	tripBreaker(failing, 4)

	if state := failing.State(); state != reliability.StateOpen {
		t.Fatalf("failing breaker state = %v, want OPEN", state)
	}

	if state := healthy.State(); state != reliability.StateClosed {
		t.Fatalf("healthy breaker state = %v, want CLOSED", state)
	}

	if set.Get("flaky_api") != failing {
		t.Fatal("set returned a new breaker for an existing key")
	}
}

func TestBreakerResetForcesClosedAndNotifiesOnce(t *testing.T) {
	t.Parallel()

	breaker := reliability.NewBreaker(breakerKey, testConfig())

	var transitions []reliability.State

	breaker.OnTransition(func(_ string, to reliability.State) {
		transitions = append(transitions, to)
	})

	tripBreaker(breaker, 4)

	if state := breaker.State(); state != reliability.StateOpen {
		t.Fatalf("state after tripping = %v, want OPEN", state)
	}

	breaker.Reset()

	if state := breaker.State(); state != reliability.StateClosed {
		t.Fatalf("state after reset = %v, want CLOSED", state)
	}

	// A second reset changes nothing, so the callback must stay quiet:
	// metrics would otherwise count transitions that never happened.
	breaker.Reset()

	want := []reliability.State{reliability.StateOpen, reliability.StateClosed}
	if !slices.Equal(transitions, want) {
		t.Fatalf("transitions = %v, want %v", transitions, want)
	}

	if err := breaker.Allow(); err != nil {
		t.Fatalf("allow after reset = %v, want calls admitted", err)
	}
}

func TestBreakerResetClearsWindowSamples(t *testing.T) {
	t.Parallel()

	breaker := reliability.NewBreaker(breakerKey, testConfig())

	// Three failures sit below MinRequests, so the breaker is still
	// CLOSED; a reset must drop them, or the next single failure would
	// trip on stale history.
	tripBreaker(breaker, 3)
	breaker.Reset()

	tripBreaker(breaker, 1)

	if state := breaker.State(); state != reliability.StateClosed {
		t.Fatalf("state = %v, want CLOSED: reset should have cleared the rolling window", state)
	}
}

func TestBreakerSetResetAndStates(t *testing.T) {
	t.Parallel()

	set := reliability.NewBreakerSet(testConfig())

	tripBreaker(set.Get("flaky_api"), 4)
	set.Get("steady_api").Record(true)

	states := set.States()
	if states["flaky_api"] != reliability.StateOpen || states["steady_api"] != reliability.StateClosed {
		t.Fatalf("states = %v, want flaky_api OPEN and steady_api CLOSED", states)
	}

	if !set.Reset("flaky_api") {
		t.Fatal("Reset(flaky_api) = false, want true for an existing breaker")
	}

	if state := set.States()["flaky_api"]; state != reliability.StateClosed {
		t.Fatalf("flaky_api after reset = %v, want CLOSED", state)
	}

	if set.Reset("ghost") {
		t.Fatal("Reset(ghost) = true, want false: unknown keys must not mint breakers")
	}

	if _, exists := set.States()["ghost"]; exists {
		t.Fatal("States() lists ghost: Reset must not create breakers")
	}
}
