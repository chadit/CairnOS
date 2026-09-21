package sweep_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/sweep"
)

// Retention windows the assertions share.
const (
	runsWindow    = 90 * 24 * time.Hour
	outputsWindow = 30 * 24 * time.Hour
)

var errPruneBroken = errors.New("prune broke")

// fakePruner reports each call's options over a channel so the test syncs
// on real activity instead of a clock.
type fakePruner struct {
	calls chan store.PruneOptions
	err   error
}

// Prune implements sweep.Pruner.
func (f *fakePruner) Prune(_ context.Context, opts store.PruneOptions) (store.PruneStats, error) {
	f.calls <- opts

	return store.PruneStats{RunsDeleted: 1}, f.err
}

// startSweep runs the loop on its own goroutine over an injected tick
// channel, returning the cancel and a done channel.
func startSweep(t *testing.T, pruner *fakePruner, ticks <-chan time.Time) (context.CancelFunc, <-chan struct{}) {
	t.Helper()

	return startSweepLogging(t, pruner, ticks, slog.DiscardHandler)
}

// startSweepLogging is startSweep with the log destination named, for a
// case whose outcome is the line an operator reads.
func startSweepLogging(
	t *testing.T, pruner *fakePruner, ticks <-chan time.Time, handler slog.Handler,
) (context.CancelFunc, <-chan struct{}) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		sweep.Run(ctx, &sweep.Config{
			Store:   pruner,
			Runs:    runsWindow,
			Outputs: outputsWindow,
			Logger:  slog.New(handler),
			Ticks:   ticks,
		})
	}()

	return cancel, done
}

// TestRunPrunesOnEveryTickAndStopsOnCancel drives two passes through the
// injected ticker, checks the options each pass carried, and proves
// cancellation ends the loop.
func TestRunPrunesOnEveryTickAndStopsOnCancel(t *testing.T) {
	t.Parallel()

	pruner := &fakePruner{calls: make(chan store.PruneOptions)}
	ticks := make(chan time.Time)
	cancel, done := startSweep(t, pruner, ticks)

	defer cancel()

	anchor := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	ticks <- anchor

	opts := <-pruner.calls
	if opts.RunsOlderThan != runsWindow || opts.OutputsOlderThan != outputsWindow {
		t.Fatalf("prune windows = %v/%v, want %v/%v",
			opts.RunsOlderThan, opts.OutputsOlderThan, runsWindow, outputsWindow)
	}

	if !opts.Now.Equal(anchor) {
		t.Fatalf("prune anchored at %v, want the tick time %v", opts.Now, anchor)
	}

	ticks <- anchor.Add(6 * time.Hour)

	second := <-pruner.calls
	if !second.Now.Equal(anchor.Add(6 * time.Hour)) {
		t.Fatalf("second pass anchored at %v, want the second tick", second.Now)
	}

	cancel()
	<-done
}

// TestRunSurvivesPruneFailures proves a failing pass logs and waits for
// the next tick instead of ending the loop. The second pass still carries
// the configured windows, so the loop resumes whole rather than degraded.
func TestRunSurvivesPruneFailures(t *testing.T) {
	t.Parallel()

	pruner := &fakePruner{calls: make(chan store.PruneOptions), err: errPruneBroken}
	ticks := make(chan time.Time)
	records := &recordingHandler{}
	cancel, done := startSweepLogging(t, pruner, ticks, records)

	defer cancel()

	ticks <- time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	<-pruner.calls

	second := time.Date(2026, 6, 1, 6, 0, 0, 0, time.UTC)
	ticks <- second

	opts := <-pruner.calls
	if !opts.Now.Equal(second) || opts.RunsOlderThan != runsWindow || opts.OutputsOlderThan != outputsWindow {
		t.Fatalf("pass after a failure ran with %+v, want the second tick and the configured windows", opts)
	}

	cancel()
	<-done

	if got := records.errors(); len(got) != 2 {
		t.Fatalf("logged %d error lines, want one per failed pass: %v", len(got), got)
	}
}

// recordingHandler keeps the error lines the sweep logs, so a case reads
// what an operator would see rather than trusting a discarded logger. The
// loop's goroutine is the only writer and reads happen after it ends.
type recordingHandler struct {
	errorMessages []string
}

// Enabled implements slog.Handler for every level, since the point is to
// see the lines a level filter would hide.
func (*recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle implements slog.Handler, keeping the error lines and dropping the
// rest.
//
//nolint:gocritic // slog.Handler fixes this signature; the record cannot be a pointer
func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Level == slog.LevelError {
		h.errorMessages = append(h.errorMessages, record.Message)
	}

	return nil
}

// WithAttrs implements slog.Handler; the sweep adds none, so the same
// handler answers.
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

// WithGroup implements slog.Handler.
func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

// errors returns the messages logged at error level.
func (h *recordingHandler) errors() []string { return h.errorMessages }

// TestRunDisabledWithoutInterval proves a zero interval and no injected
// ticker returns immediately rather than spinning or blocking.
func TestRunDisabledWithoutInterval(t *testing.T) {
	t.Parallel()

	pruner := &fakePruner{calls: make(chan store.PruneOptions, 1)}

	sweep.Run(t.Context(), &sweep.Config{
		Store:  pruner,
		Logger: slog.New(slog.DiscardHandler),
	})

	select {
	case opts := <-pruner.calls:
		t.Fatalf("disabled sweep still pruned with %+v", opts)
	default:
	}
}
