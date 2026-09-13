package notify_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/notify"
)

// errSinkDown stands in for any delivery failure a sink can hit.
var errSinkDown = errors.New("sink is down")

// recordingPoster captures what one sink was handed, so a test can prove the
// fan-out routed a line to the right destinations. The channel is buffered
// because Post runs on the drainer goroutine and no test reads in lockstep.
type recordingPoster struct {
	posted chan string
	err    error
}

func newRecordingPoster(err error) *recordingPoster {
	return &recordingPoster{posted: make(chan string, 16), err: err}
}

func (p *recordingPoster) Post(_ context.Context, content string) error {
	p.posted <- content

	return p.err
}

// awaitPost waits for the next line this sink received, failing the test rather
// than hanging when the fan-out never delivered one.
func (p *recordingPoster) awaitPost(t *testing.T) string {
	t.Helper()

	select {
	case got := <-p.posted:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a sink to receive a line")

		return ""
	}
}

// expectNoPost proves a sink was left out of a delivery.
func (p *recordingPoster) expectNoPost(t *testing.T) {
	t.Helper()

	select {
	case got := <-p.posted:
		t.Fatalf("sink received %q, want nothing", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestFanoutDeliversToEverySinkThatSelectedTheEvent(t *testing.T) {
	t.Parallel()

	// Two sinks with overlapping-but-different event sets: held goes to both,
	// budget_denied only to the second. This is the FR34 shape, a Discord
	// webhook and a Mattermost channel subscribed to different slices.
	both := newRecordingPoster(nil)
	budgetOnly := newRecordingPoster(nil)

	fanout := notify.NewFanout(nil,
		notify.Sink{Poster: both, Events: []string{eventHeld}},
		notify.Sink{Poster: budgetOnly, Events: []string{eventHeld, eventBudget}},
	)

	runFanout(t, fanout)

	fanout.Emit(t.Context(), eventHeld, lineHeld)

	if got := both.awaitPost(t); got != lineHeld {
		t.Errorf("first sink got %q, want %q", got, lineHeld)
	}

	if got := budgetOnly.awaitPost(t); got != lineHeld {
		t.Errorf("second sink got %q, want %q", got, lineHeld)
	}

	fanout.Emit(t.Context(), eventBudget, lineBudget)

	if got := budgetOnly.awaitPost(t); got != lineBudget {
		t.Errorf("second sink got %q, want %q", got, lineBudget)
	}

	both.expectNoPost(t)
}

func TestFanoutFailingSinkStillDeliversToTheOthers(t *testing.T) {
	t.Parallel()

	// A Mattermost identity that never registered fails every post. The Discord
	// webhook beside it must still receive the line, which is the whole reason
	// each sink is attempted independently.
	failing := newRecordingPoster(errSinkDown)
	healthy := newRecordingPoster(nil)

	fanout := notify.NewFanout(nil,
		notify.Sink{Poster: failing, Events: []string{eventHeld}},
		notify.Sink{Poster: healthy, Events: []string{eventHeld}},
	)

	runFanout(t, fanout)

	fanout.Emit(t.Context(), eventHeld, lineHeld)

	if got := failing.awaitPost(t); got != lineHeld {
		t.Errorf("failing sink got %q, want %q", got, lineHeld)
	}

	if got := healthy.awaitPost(t); got != lineHeld {
		t.Errorf("healthy sink got %q, want %q", got, lineHeld)
	}

	// The drainer must survive the failure and keep posting.
	fanout.Emit(t.Context(), eventHeld, lineRunFailed)

	if got := healthy.awaitPost(t); got != lineRunFailed {
		t.Errorf("healthy sink got %q after a sibling failure, want %q", got, lineRunFailed)
	}
}

func TestFanoutSelectsWhenAnySinkWantsTheEvent(t *testing.T) {
	t.Parallel()

	// Selects is the producer's "should I render this line at all" check, so it
	// has to be the union across sinks, not the first sink's set.
	fanout := notify.NewFanout(nil,
		notify.Sink{Poster: newRecordingPoster(nil), Events: []string{eventHeld}},
		notify.Sink{Poster: newRecordingPoster(nil), Events: []string{eventBudget}},
	)

	for _, event := range []string{eventHeld, eventBudget} {
		if !fanout.Selects(event) {
			t.Errorf("Selects(%q) is false; some sink wants it", event)
		}
	}

	if fanout.Selects(eventDeadLetter) {
		t.Errorf("Selects(%q) is true; no sink wants it", eventDeadLetter)
	}
}

func TestFanoutWithNoSinksSelectsNothing(t *testing.T) {
	t.Parallel()

	// Serve returns a nil fan-out when nothing is configured, but a constructed
	// one with an empty sink list must be inert the same way rather than panic.
	fanout := notify.NewFanout(nil)

	if fanout.Selects(eventHeld) {
		t.Error("a fan-out with no sinks selects nothing")
	}

	runFanout(t, fanout)
	fanout.Emit(t.Context(), eventHeld, lineHeld)
}
