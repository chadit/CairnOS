package cli_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/notify"
	"github.com/chadit/CairnOS/internal/scheduler"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/pkg/runner"
)

const fanoutTestWorkflow = "wf"

// fanoutCaptureServer answers every webhook POST 204 and pushes the decoded
// content string onto the returned channel, so a test sees exactly what a
// producer fanned out.
func fanoutCaptureServer(t *testing.T) (*httptest.Server, chan string) {
	t.Helper()

	bodies := make(chan string, 8)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)

		var payload struct {
			Content string `json:"content"`
		}

		_ = json.Unmarshal(raw, &payload)
		bodies <- payload.Content

		w.WriteHeader(http.StatusNoContent)
	}))

	t.Cleanup(server.Close)

	return server, bodies
}

// drainFanout starts the fan-out drainer on a canceled-on-cleanup context.
func drainFanout(t *testing.T, fanout *notify.Fanout) context.Context {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := cli.StartFanout(ctx, fanout)

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return ctx
}

// assertNoSecondPost fails if a second body arrives, proving a producer emitted
// exactly one line.
func assertNoSecondPost(t *testing.T, bodies <-chan string) {
	t.Helper()

	select {
	case got := <-bodies:
		t.Fatalf("a second line posted unexpectedly: %q", got)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestReauthOnHoldRoutesHeldByteIdentical proves the default ["held"] config
// routes a held alert through the fan-out with a body byte-identical to the
// direct Notify path, so serve keeps today's re-auth hold message
// (.agents/spec/parity/messaging-bridge.md, decision 7, acceptance 7).
func TestReauthOnHoldRoutesHeldByteIdentical(t *testing.T) {
	// t.Setenv forbids t.Parallel; the webhook env is process-global.
	server, bodies := fanoutCaptureServer(t)
	t.Setenv(cli.NotifyDiscordWebhookEnv, server.URL)

	cfg := config.Default()
	logger := slog.New(slog.DiscardHandler)

	fanout := cli.BuildDiscordFanout(&cfg, logger)
	if fanout == nil {
		t.Fatal("cli.BuildDiscordFanout returned nil with the webhook env set")
	}

	onHold := cli.NewReauthOnHold(fanout, logger)
	if onHold == nil {
		t.Fatal("cli.NewReauthOnHold returned nil with the default held event selected")
	}

	ctx := drainFanout(t, fanout)

	item := &scheduler.WorkItem{ID: "wi-1", DedupKey: "sched-9|2026-07-04T00:00:00Z"}
	// A code whose tool is not a known re-auth provider keeps Account empty, so
	// the expected line does not depend on the test host's credential files.
	onHold(ctx, item, runner.NewReauthError("faketool.needs_reauth", "run: faketool login"))

	want := notify.ReauthAlert{
		DedupKey:    item.DedupKey,
		Tool:        "faketool",
		Remediation: "run: faketool login",
	}.Message()

	select {
	case got := <-bodies:
		if got != want {
			t.Fatalf("held line = %q, want byte-identical %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the held alert to post")
	}
}

// TestReauthOnHoldNilWhenHeldDeselected proves an operator who drops "held" from
// the event set gets no held callback, so the hold path is only logged and
// counted.
func TestReauthOnHoldNilWhenHeldDeselected(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	fanout := notify.NewFanout(notify.NewDiscordNotifier("http://example.invalid"),
		[]string{config.NotifyEventRunFailed}, logger)

	if onHold := cli.NewReauthOnHold(fanout, logger); onHold != nil {
		t.Fatal("cli.NewReauthOnHold returned a callback with held deselected")
	}

	if onHold := cli.NewReauthOnHold(nil, logger); onHold != nil {
		t.Fatal("cli.NewReauthOnHold returned a callback with the fan-out off")
	}
}

// TestWrapRunFailedFanoutEmitsOnFailedFinish proves the OnEvent wrapper always
// dispatches and emits a run_failed line only on a failed run finish, naming the
// run and its error.
func TestWrapRunFailedFanoutEmitsOnFailedFinish(t *testing.T) {
	t.Parallel()

	server, bodies := fanoutCaptureServer(t)
	logger := slog.New(slog.DiscardHandler)
	fanout := notify.NewFanout(notify.NewDiscordNotifier(server.URL),
		[]string{config.NotifyEventRunFailed}, logger)

	ctx := drainFanout(t, fanout)

	var (
		dispatchMu sync.Mutex
		dispatched int
	)

	dispatch := func(engine.Event) {
		dispatchMu.Lock()
		dispatched++
		dispatchMu.Unlock()
	}

	onEvent := cli.WrapRunFailedFanout(ctx, dispatch, fanout)

	// A start and a successful finish dispatch but emit nothing.
	onEvent(engine.Event{RunID: "r-1", Type: engine.EventRunStarted})
	onEvent(engine.Event{RunID: "r-1", Type: engine.EventRunFinished, Status: string(store.RunSucceeded)})
	// A failed finish emits one line carrying the run ID and the error.
	onEvent(engine.Event{
		RunID:  "r-2",
		Type:   engine.EventRunFinished,
		Status: string(store.RunFailed),
		Err:    cairnerr.New("action.timeout", "boom"),
	})

	select {
	case got := <-bodies:
		for _, want := range []string{"r-2", "action.timeout", "boom"} {
			if !strings.Contains(got, want) {
				t.Fatalf("run_failed line %q missing %q", got, want)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the run_failed line")
	}

	assertNoSecondPost(t, bodies)

	dispatchMu.Lock()
	defer dispatchMu.Unlock()

	if dispatched != 3 {
		t.Errorf("dispatch called %d times, want 3 (the wrapper never drops an event)", dispatched)
	}
}

// TestBudgetFanoutNotifierEmitsDeniedNotWarn proves the pause-mode denial fans a
// budget_denied line out while the warn path only logs, so warn-mode behavior is
// unchanged.
func TestBudgetFanoutNotifierEmitsDeniedNotWarn(t *testing.T) {
	t.Parallel()

	server, bodies := fanoutCaptureServer(t)
	logger := slog.New(slog.DiscardHandler)
	fanout := notify.NewFanout(notify.NewDiscordNotifier(server.URL),
		[]string{config.NotifyEventBudgetDenied}, logger)

	ctx := drainFanout(t, fanout)

	notifier := cli.NewBudgetFanoutNotifier(fanout, logger)
	if notifier == nil {
		t.Fatal("cli.NewBudgetFanoutNotifier returned nil with the fan-out on")
	}

	// Warn only logs: there is no warn event, so nothing posts.
	notifier.BudgetWarn(ctx, fanoutTestWorkflow, 12.0, 10.0)
	notifier.BudgetDenied(ctx, fanoutTestWorkflow, 12.0, 10.0)

	select {
	case got := <-bodies:
		for _, want := range []string{fanoutTestWorkflow, "12.00", "10.00"} {
			if !strings.Contains(got, want) {
				t.Fatalf("budget_denied line %q missing %q", got, want)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the budget_denied line")
	}

	assertNoSecondPost(t, bodies)
}

// TestNewBudgetFanoutNotifierNilWhenFanoutOff proves a fan-out-less serve keeps
// the engine's own slog warn fallback and never wires the notifier.
func TestNewBudgetFanoutNotifierNilWhenFanoutOff(t *testing.T) {
	t.Parallel()

	if notifier := cli.NewBudgetFanoutNotifier(nil, slog.New(slog.DiscardHandler)); notifier != nil {
		t.Fatal("cli.NewBudgetFanoutNotifier returned a notifier with the fan-out off")
	}
}

// TestNewDeadLetterFanoutEmitsWhenSelected proves the dead-letter callback fans a
// line out for a settled-dead item, and is nil when the event is deselected.
func TestNewDeadLetterFanoutEmitsWhenSelected(t *testing.T) {
	t.Parallel()

	server, bodies := fanoutCaptureServer(t)
	logger := slog.New(slog.DiscardHandler)
	fanout := notify.NewFanout(notify.NewDiscordNotifier(server.URL),
		[]string{config.NotifyEventDeadLettered}, logger)

	ctx := drainFanout(t, fanout)

	onDeadLetter := cli.NewDeadLetterFanout(fanout)
	if onDeadLetter == nil {
		t.Fatal("cli.NewDeadLetterFanout returned nil with dead_lettered selected")
	}

	item := &scheduler.WorkItem{ID: "wi-2", DedupKey: "sched-3|fire", Deliveries: 5}
	onDeadLetter(ctx, item, cairnerr.New("action.failed", "gave up"))

	select {
	case got := <-bodies:
		if !strings.Contains(got, item.DedupKey) {
			t.Fatalf("dead_lettered line %q missing the dedup key", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the dead_lettered line")
	}

	if off := cli.NewDeadLetterFanout(nil); off != nil {
		t.Fatal("cli.NewDeadLetterFanout returned a callback with the fan-out off")
	}
}
