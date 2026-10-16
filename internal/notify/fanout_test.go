package notify_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/nettest"
	"github.com/chadit/CairnOS/internal/notify"
)

// Fan-out event names and lines, hoisted so the repeated literals do not trip
// goconst and every case reads the same value the assertion checks.
const (
	eventHeld        = "held"
	eventRunFailed   = "run_failed"
	eventDeadLetter  = "dead_lettered"
	eventBudget      = "budget_denied"
	lineHeld         = "job held for re-auth"
	lineRunFailed    = "run r-1 failed"
	lineDeadLettered = "item i-1 dead-lettered"
	lineBudget       = "workflow over budget"
)

// captureServer answers every webhook POST with status and pushes the decoded
// content string onto the returned channel, so a test observes what the fan-out
// actually posted.
func captureServer(t *testing.T, status int) (*nettest.Fake, chan string) {
	t.Helper()

	bodies := make(chan string, 16)

	server := nettest.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Content string `json:"content"`
		}

		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode webhook body: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		bodies <- payload.Content

		w.WriteHeader(status)
	}))

	return server, bodies
}

// discordSink builds the one-sink slice most of these cases want: a Discord
// notifier pointed at the capture server, subscribed to the named events.
func discordSink(server *nettest.Fake, events ...string) notify.Sink {
	return notify.Sink{
		Poster: notify.NewDiscordNotifierOver(server.URL, server.Transport()),
		Events: events,
	}
}

// runFanout starts the drainer on a canceled-on-cleanup context.
func runFanout(t *testing.T, fanout *notify.Fanout) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	go fanout.Run(ctx)
}

func TestFanoutFiltersUnselectedEvents(t *testing.T) {
	t.Parallel()

	server, bodies := captureServer(t, http.StatusNoContent)
	fanout := notify.NewFanout(nil, discordSink(server, eventHeld))

	runFanout(t, fanout)

	// An unselected event never enqueues, so the drainer only ever posts the
	// selected one; the first (and only) body must be the held line.
	fanout.Emit(t.Context(), eventRunFailed, lineRunFailed)
	fanout.Emit(t.Context(), eventHeld, lineHeld)

	select {
	case got := <-bodies:
		if got != lineHeld {
			t.Fatalf("posted %q, want the held line %q", got, lineHeld)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the selected event to post")
	}

	select {
	case got := <-bodies:
		t.Fatalf("unselected event posted %q, want nothing", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestFanoutPostsOneLinePerSelectedEvent(t *testing.T) {
	t.Parallel()

	server, bodies := captureServer(t, http.StatusNoContent)
	fanout := notify.NewFanout(nil,
		discordSink(server, eventHeld, eventRunFailed, eventDeadLetter, eventBudget),
	)

	runFanout(t, fanout)

	want := map[string]string{
		eventHeld:       lineHeld,
		eventRunFailed:  lineRunFailed,
		eventDeadLetter: lineDeadLettered,
		eventBudget:     lineBudget,
	}
	for event, line := range want {
		fanout.Emit(t.Context(), event, line)
	}

	got := make(map[string]bool, len(want))
	for range want {
		select {
		case body := <-bodies:
			got[body] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out; received %d of %d lines", len(got), len(want))
		}
	}

	for _, line := range want {
		if !got[line] {
			t.Errorf("line %q never posted", line)
		}
	}
}

func TestFanoutDropsOnOverflowWithoutBlocking(t *testing.T) {
	t.Parallel()

	// No drainer runs, so the buffer fills and never empties. Emit past capacity
	// must still return promptly rather than block the producer goroutine.
	server, _ := captureServer(t, http.StatusNoContent)
	fanout := notify.NewFanout(nil, discordSink(server, eventHeld))

	ctx := t.Context()
	done := make(chan struct{})

	go func() {
		defer close(done)

		for range 512 {
			fanout.Emit(ctx, eventHeld, lineHeld)
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked once the buffer filled; it must drop instead")
	}
}

func TestFanoutWebhookFailurePropagatesNothing(t *testing.T) {
	t.Parallel()

	// A failing webhook must not surface anywhere: Emit is fire-and-forget and
	// the drainer swallows the post error. The producer sees no panic and no
	// block, and the webhook is still attempted.
	server, bodies := captureServer(t, http.StatusInternalServerError)
	fanout := notify.NewFanout(nil, discordSink(server, eventBudget))

	runFanout(t, fanout)

	fanout.Emit(t.Context(), eventBudget, lineBudget)

	select {
	case got := <-bodies:
		if got != lineBudget {
			t.Fatalf("webhook saw %q, want %q", got, lineBudget)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the failing webhook to be attempted")
	}
}

func TestFanoutNilIsInert(t *testing.T) {
	t.Parallel()

	// The nil fan-out is the no-webhook state; every method is a safe no-op so
	// serve wires producers without a nil guard.
	var fanout *notify.Fanout

	if fanout.Selects(eventHeld) {
		t.Error("nil fan-out selects nothing")
	}

	fanout.Emit(t.Context(), eventHeld, lineHeld)
	fanout.Run(t.Context())
}

// TestFanoutMatchesNotifyBody proves the held line the fan-out posts is
// byte-identical to what the direct Notify path produced, so routing held
// through the fan-out keeps the default set unchanged.
func TestFanoutMatchesNotifyBody(t *testing.T) {
	t.Parallel()

	server, bodies := captureServer(t, http.StatusNoContent)
	notifier := notify.NewDiscordNotifierOver(server.URL, server.Transport())

	alert := notify.ReauthAlert{
		Tool:        toolCodex,
		Remediation: alertRemediation,
		DedupKey:    alertDedupKey,
	}

	if err := notifier.Notify(t.Context(), alert); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	direct := <-bodies

	fanout := notify.NewFanout(nil, notify.Sink{Poster: notifier, Events: []string{eventHeld}})
	runFanout(t, fanout)
	fanout.Emit(t.Context(), eventHeld, alert.Message())

	select {
	case routed := <-bodies:
		if routed != direct {
			t.Fatalf("fan-out posted %q, want the direct body %q", routed, direct)
		}

		if !strings.Contains(routed, toolCodex) {
			t.Fatalf("held line dropped the tool name: %q", routed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the routed held line")
	}
}
