package scheduler_test

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/scheduler"
)

// recordingQueueRunner records the items it is asked to run and optionally fails
// every one, so a test can assert delivery counts and the success or failure
// path.
type recordingQueueRunner struct {
	mu       sync.Mutex
	seen     []string
	failAll  bool
	failWith error
}

// Run records the item and fails per the configured mode: a specific error when
// failWith is set, a generic one when failAll is set, else success.
func (r *recordingQueueRunner) Run(_ context.Context, item *scheduler.WorkItem) error {
	r.mu.Lock()
	r.seen = append(r.seen, item.DedupKey)
	r.mu.Unlock()

	if r.failWith != nil {
		return r.failWith
	}

	if r.failAll {
		return errors.New("boom")
	}

	return nil
}

// count reports how many runs have happened.
func (r *recordingQueueRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.seen)
}

// signalMetrics turns the worker's metric callbacks into channel sends, so a
// caller synchronizes on real worker progress (an ack, a dead-letter) instead of
// polling with sleeps. The channels are buffered so the worker never blocks on a
// signal nobody has read yet.
type signalMetrics struct {
	runs       chan bool
	deadLetter chan struct{}
	held       chan struct{}
	resumed    chan struct{}
}

func newSignalMetrics() *signalMetrics {
	return &signalMetrics{
		runs:       make(chan bool, 128),
		deadLetter: make(chan struct{}, 8),
		held:       make(chan struct{}, 8),
		resumed:    make(chan struct{}, 8),
	}
}

func (m *signalMetrics) ItemRun(ok bool) {
	m.runs <- ok
}

func (m *signalMetrics) ItemDeadLettered() {
	m.deadLetter <- struct{}{}
}

func (m *signalMetrics) ItemHeld() {
	m.held <- struct{}{}
}

func (m *signalMetrics) ItemResumed() {
	m.resumed <- struct{}{}
}

func (*signalMetrics) LeaseReclaimed(_ int) {}

func (*signalMetrics) Depth(_ scheduler.QueueStats) {}

func TestWorkerProcessesAndAcks(t *testing.T) {
	t.Parallel()

	queue := openWorkerQueue(t)
	metrics := newSignalMetrics()

	const total = 5
	for idx := range total {
		enqueueWorkerItem(t, queue, idx)
	}

	runner := &recordingQueueRunner{}
	worker := scheduler.NewWorker(&scheduler.WorkerConfig{
		Queue:         queue,
		Runner:        runner,
		Concurrency:   2,
		Prefetch:      4,
		PollInterval:  5 * time.Millisecond,
		SweepInterval: time.Hour,
		Logger:        discardLogger(),
		Metrics:       metrics,
	})

	stop := runWorker(t, worker)
	defer stop()

	// report(true) fires after the ack, so receiving total success signals proves
	// every item ran once and was removed from the queue.
	for idx := range total {
		select {
		case ok := <-metrics.runs:
			if !ok {
				t.Errorf("item %d naked, want acked", idx)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for item %d", idx)
		}
	}

	if got := runner.count(); got != total {
		t.Errorf("runner saw %d items, want %d", got, total)
	}

	stats, err := queue.Stats(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	if stats.Ready+stats.Leased != 0 {
		t.Errorf("queue not drained: %+v", stats)
	}
}

func TestWorkerDeadLettersAfterMaxDeliveries(t *testing.T) {
	t.Parallel()

	queue := openWorkerQueue(t)
	metrics := newSignalMetrics()

	enqueueWorkerItem(t, queue, 0)

	runner := &recordingQueueRunner{failAll: true}
	worker := scheduler.NewWorker(&scheduler.WorkerConfig{
		Queue:         queue,
		Runner:        runner,
		Concurrency:   1,
		Prefetch:      1,
		Backoff:       time.Millisecond,
		MaxDeliveries: 2,
		PollInterval:  2 * time.Millisecond,
		SweepInterval: time.Hour,
		Logger:        discardLogger(),
		Metrics:       metrics,
	})

	stop := runWorker(t, worker)
	defer stop()

	select {
	case <-metrics.deadLetter:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for dead-letter")
	}

	if got := runner.count(); got < 2 {
		t.Errorf("runner saw %d deliveries before dead-letter, want at least 2", got)
	}
}

func TestWorkerHoldsHoldableError(t *testing.T) {
	t.Parallel()

	queue := openWorkerQueue(t)
	metrics := newSignalMetrics()

	enqueueWorkerItem(t, queue, 0)

	reauthErr := errors.New("needs reauth")
	runner := &recordingQueueRunner{failWith: reauthErr}

	// notified carries the item OnHold saw, so the test proves the worker calls
	// the operator-alert hook after parking the job (D12 slice 3).
	notified := make(chan string, 1)

	worker := scheduler.NewWorker(&scheduler.WorkerConfig{
		Queue:         queue,
		Runner:        runner,
		Concurrency:   1,
		Prefetch:      1,
		Backoff:       time.Millisecond,
		MaxDeliveries: 2,
		PollInterval:  2 * time.Millisecond,
		SweepInterval: time.Hour,
		Logger:        discardLogger(),
		Metrics:       metrics,
		Holdable:      func(err error) bool { return errors.Is(err, reauthErr) },
		HoldKey:       func(error) string { return testHoldKey },
		OnHold: func(_ context.Context, item *scheduler.WorkItem, _ error) {
			notified <- item.DedupKey
		},
	})

	stop := runWorker(t, worker)
	defer stop()

	select {
	case <-metrics.held:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for hold")
	}

	select {
	case key := <-notified:
		if key != "item-a" {
			t.Errorf("OnHold saw dedup %q, want item-a", key)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for OnHold")
	}

	// A held item is parked, not claimable and not dead-lettered: a human
	// re-auths and resumes it later.
	stats, err := queue.Stats(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	if stats.Held != 1 || stats.Dead != 0 {
		t.Errorf("stats = %+v, want one held and zero dead", stats)
	}

	// The worker stored the HoldKey predicate's result on the held item, so a
	// resume probe can later read which credential it waits on (D12).
	listed, err := queue.ListHeld(t.Context())
	if err != nil {
		t.Fatalf("list held: %v", err)
	}

	if len(listed) != 1 || listed[0].HoldKey != testHoldKey {
		t.Errorf("ListHeld = %+v, want one item with hold key codex", listed)
	}
}

func TestWorkerResumesValidHeldItem(t *testing.T) {
	t.Parallel()

	queue := openWorkerQueue(t)
	metrics := newSignalMetrics()

	// Park an item in the held state before the worker starts, so the only way
	// the runner ever sees it is the resume sweep returning it to ready.
	enqueueWorkerItem(t, queue, 0)

	claimed, err := queue.Claim(t.Context(), time.Now(), time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("seed claim: got %d items, err %v", len(claimed), err)
	}

	if holdErr := queue.Hold(t.Context(), claimed[0].ID, testHoldKey); holdErr != nil {
		t.Fatalf("seed hold: %v", holdErr)
	}

	runner := &recordingQueueRunner{}
	worker := scheduler.NewWorker(&scheduler.WorkerConfig{
		Queue:         queue,
		Runner:        runner,
		Concurrency:   1,
		Prefetch:      1,
		PollInterval:  2 * time.Millisecond,
		SweepInterval: 5 * time.Millisecond,
		Logger:        discardLogger(),
		Metrics:       metrics,
		ResumeProbe:   func(holdKey string, _ time.Time) bool { return holdKey == testHoldKey },
	})

	stop := runWorker(t, worker)
	defer stop()

	// The sweep counts the resume before the worker re-claims the item.
	select {
	case <-metrics.resumed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the resume signal")
	}

	// A run signal can only arrive if the sweep resumed the held item and the
	// worker then claimed and acked it.
	select {
	case ok := <-metrics.runs:
		if !ok {
			t.Error("resumed item naked, want acked")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the resumed item to run")
	}
}

// openWorkerQueue opens a fresh SQLite-backed queue for a worker test.
func openWorkerQueue(t *testing.T) *scheduler.SQLiteQueue {
	t.Helper()

	queue, err := scheduler.OpenSQLiteQueue(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open sqlite queue: %v", err)
	}

	t.Cleanup(func() { _ = queue.Close() })

	return queue
}

// enqueueWorkerItem enqueues one ready item available now.
func enqueueWorkerItem(t *testing.T, queue scheduler.WorkQueue, idx int) {
	t.Helper()

	if _, err := queue.Enqueue(t.Context(), &scheduler.WorkItem{
		DedupKey: "item-" + string(rune('a'+idx)),
		Payload:  []byte(`{"workflow":"x"}`),
	}); err != nil {
		t.Fatalf("enqueue %d: %v", idx, err)
	}
}

// runWorker starts the worker on a cancelable context and returns a stop
// function that cancels it and waits for a clean exit.
func runWorker(t *testing.T, worker *scheduler.Worker) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		if err := worker.Run(ctx); err != nil {
			t.Errorf("worker run: %v", err)
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

// discardLogger is a slog logger that drops output, keeping test logs quiet.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
