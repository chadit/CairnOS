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
	failWith error
	seen     []string
	mu       sync.Mutex
	failAll  bool
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

const (
	// runSignalBufferSize holds every ItemRun signal a worker test produces, so
	// the worker never blocks on a run nobody has read yet.
	runSignalBufferSize = 128

	// eventSignalBufferSize leaves room for more dead-letter, hold, and resume
	// signals than a test expects, so a wrongly repeated one is observable
	// rather than blocking the worker.
	eventSignalBufferSize = 8
)

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
		runs:       make(chan bool, runSignalBufferSize),
		deadLetter: make(chan struct{}, eventSignalBufferSize),
		held:       make(chan struct{}, eventSignalBufferSize),
		resumed:    make(chan struct{}, eventSignalBufferSize),
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

// TestWorkerOnDeadLetterFiresOnlyOnDeadLetter proves OnDeadLetter fires on the
// terminal dead-letter and not on the intermediate requeue nak, so the Discord
// fan-out posts one dead_lettered line per item, not one per delivery.
func TestWorkerOnDeadLetterFiresOnlyOnDeadLetter(t *testing.T) {
	t.Parallel()

	queue := openWorkerQueue(t)
	metrics := newSignalMetrics()

	enqueueWorkerItem(t, queue, 0)

	runner := &recordingQueueRunner{failAll: true}

	// deadLettered carries the item each OnDeadLetter call saw. Buffered wide so a
	// wrongly-repeated call would be observable rather than blocking.
	deadLettered := make(chan string, eventSignalBufferSize)

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
		OnDeadLetter: func(_ context.Context, item *scheduler.WorkItem, _ error) {
			deadLettered <- item.DedupKey
		},
	})

	stop := runWorker(t, worker)
	defer stop()

	select {
	case <-metrics.deadLetter:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for dead-letter")
	}

	select {
	case key := <-deadLettered:
		if key != firstItemDedup {
			t.Errorf("OnDeadLetter saw dedup %q, want %q", key, firstItemDedup)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for OnDeadLetter")
	}

	// The first delivery naked into a requeue without firing OnDeadLetter, so no
	// second call arrives even though the runner ran at least twice.
	select {
	case key := <-deadLettered:
		t.Fatalf("OnDeadLetter fired twice (saw %q); it must fire only on the terminal dead-letter", key)
	case <-time.After(100 * time.Millisecond):
	}

	if got := runner.count(); got < 2 {
		t.Errorf("runner saw %d deliveries, want at least 2 before dead-letter", got)
	}
}

// TestWorkerOnDeadLetterSkippedOnHold proves a held item never reaches
// OnDeadLetter: the hold path settles the item without dead-lettering, so no
// dead_lettered line is fanned out for a job parked for re-auth.
func TestWorkerOnDeadLetterSkippedOnHold(t *testing.T) {
	t.Parallel()

	queue := openWorkerQueue(t)
	metrics := newSignalMetrics()

	enqueueWorkerItem(t, queue, 0)

	reauthErr := errors.New("needs reauth")
	runner := &recordingQueueRunner{failWith: reauthErr}
	deadLettered := make(chan string, oneShotChanSize)

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
		OnDeadLetter: func(_ context.Context, item *scheduler.WorkItem, _ error) {
			deadLettered <- item.DedupKey
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
	case key := <-deadLettered:
		t.Fatalf("OnDeadLetter fired for a held item (saw %q); a hold is not a dead-letter", key)
	case <-time.After(100 * time.Millisecond):
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
	// the operator-alert hook after parking the job.
	notified := make(chan string, oneShotChanSize)

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
		if key != firstItemDedup {
			t.Errorf("OnHold saw dedup %q, want %q", key, firstItemDedup)
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
	// resume probe can later read which credential it waits on.
	listed, err := queue.ListHeld(t.Context())
	if err != nil {
		t.Fatalf("list held: %v", err)
	}

	if len(listed) != 1 || listed[0].HoldKey != testHoldKey {
		t.Errorf("ListHeld = %+v, want one item with hold key codex", listed)
	}
}

// TestWorkerFailsHoldableErrorWithoutHoldKey pins the keyless-hold guard: a
// re-auth failure from a tool no probe knows names no credential, so holding it
// would park the occurrence forever and leave the overlap guard shut.
func TestWorkerFailsHoldableErrorWithoutHoldKey(t *testing.T) {
	t.Parallel()

	queue := openWorkerQueue(t)
	metrics := newSignalMetrics()

	const scheduleID = "unknown-tool-sched"

	dedup := scheduler.DedupKeyFor(scheduleID, time.Now())
	if _, err := queue.Enqueue(t.Context(), &scheduler.WorkItem{
		DedupKey: dedup,
		Payload:  []byte(`{"workflow":"x"}`),
	}); err != nil {
		t.Fatalf("enqueue occurrence: %v", err)
	}

	reauthErr := errors.New("needs reauth: gizmo")
	runner := &recordingQueueRunner{failWith: reauthErr}

	// heldNotice catches any OnHold call, so the test proves the worker never
	// parked the item behind the operator's back.
	heldNotice := make(chan string, oneShotChanSize)

	worker := scheduler.NewWorker(&scheduler.WorkerConfig{
		Queue:         queue,
		Runner:        runner,
		Concurrency:   1,
		Prefetch:      1,
		Backoff:       time.Millisecond,
		MaxDeliveries: 1,
		PollInterval:  2 * time.Millisecond,
		SweepInterval: time.Hour,
		Logger:        discardLogger(),
		Metrics:       metrics,
		Holdable:      func(err error) bool { return errors.Is(err, reauthErr) },
		HoldKey:       func(error) string { return "" },
		OnHold: func(_ context.Context, item *scheduler.WorkItem, _ error) {
			heldNotice <- item.DedupKey
		},
	})

	stop := runWorker(t, worker)
	defer stop()

	select {
	case <-metrics.deadLetter:
	case <-metrics.held:
		t.Fatal("worker held an item whose hold key is empty")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the dead-letter")
	}

	select {
	case key := <-heldNotice:
		t.Fatalf("OnHold fired for %q; a keyless hold is a failure, not a hold", key)
	case <-time.After(100 * time.Millisecond):
	}

	stats, err := queue.Stats(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	if stats.Held != 0 || stats.Dead != 1 {
		t.Errorf("stats = %+v, want zero held and one dead", stats)
	}

	// The whole point: a dead occurrence is finished, so the next fire is free to
	// start rather than being skipped by the overlap guard forever.
	pending, err := queue.OccurrencePending(t.Context(), scheduleID, time.Now())
	if err != nil {
		t.Fatalf("occurrence pending: %v", err)
	}

	if pending {
		t.Error("occurrence still reads as pending after the item dead-lettered")
	}
}

// TestWorkerReleasesKeylessHeldItem covers the upgrade path: a host that ran an
// older build can already hold a keyless row, and the sweep has to let it go even
// with auto-resume off.
func TestWorkerReleasesKeylessHeldItem(t *testing.T) {
	t.Parallel()

	queue := openWorkerQueue(t)
	metrics := newSignalMetrics()

	enqueueWorkerItem(t, queue, 0)

	claimed, err := queue.Claim(t.Context(), time.Now(), time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("seed claim: got %d items, err %v", len(claimed), err)
	}

	if holdErr := queue.Hold(t.Context(), claimed[0].ID, ""); holdErr != nil {
		t.Fatalf("seed keyless hold: %v", holdErr)
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
	})

	stop := runWorker(t, worker)
	defer stop()

	// A run signal can only arrive if the sweep released the keyless hold and the
	// worker then claimed and acked the item; no resume probe is wired.
	select {
	case ok := <-metrics.runs:
		if !ok {
			t.Error("released item naked, want acked")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the released item to run")
	}

	stats, err := queue.Stats(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	if stats.Held != 0 {
		t.Errorf("stats = %+v, want nothing left held", stats)
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

// firstItemDedup is the dedup key enqueueWorkerItem gives index 0.
const firstItemDedup = "item-a"

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

// Heartbeat fixtures. The lease is short enough that the worker beats several
// times inside the test's patience, and the poll is a quarter of it so a
// renewal is never missed between reads.
const (
	heartbeatSchedule = "heartbeat-sched"
	heartbeatLease    = 100 * time.Millisecond
	heartbeatPoll     = heartbeatLease / 4
	heartbeatPatience = 5 * time.Second
)

// blockingQueueRunner holds its item until the test releases it or the worker
// stops, so a case can read the queue while a run is genuinely in flight.
type blockingQueueRunner struct {
	started chan struct{}
	release chan struct{}
}

func (r *blockingQueueRunner) Run(ctx context.Context, _ *scheduler.WorkItem) error {
	r.started <- struct{}{}

	select {
	case <-r.release:
	case <-ctx.Done():
	}

	return nil
}

// TestWorkerRenewsLeasesWhileRunning pins the heartbeat. A run outlasting
// ack_wait keeps hold of its item, so the overlap guard still reads the
// occurrence as in flight rather than letting a second one start on top of it.
func TestWorkerRenewsLeasesWhileRunning(t *testing.T) {
	t.Parallel()

	queue := openWorkerQueue(t)

	if _, err := queue.Enqueue(t.Context(), &scheduler.WorkItem{
		DedupKey: scheduler.DedupKeyFor(heartbeatSchedule, time.Now()),
		Payload:  []byte(`{"workflow":"x"}`),
	}); err != nil {
		t.Fatalf("enqueue occurrence: %v", err)
	}

	runner := &blockingQueueRunner{started: make(chan struct{}, oneShotChanSize), release: make(chan struct{})}
	worker := scheduler.NewWorker(&scheduler.WorkerConfig{
		Queue:         queue,
		Runner:        runner,
		AckWait:       heartbeatLease,
		PollInterval:  5 * time.Millisecond,
		SweepInterval: time.Hour,
		Logger:        discardLogger(),
	})

	stop := runWorker(t, worker)

	defer func() {
		close(runner.release)
		stop()
	}()

	select {
	case <-runner.started:
	case <-time.After(heartbeatPatience):
		t.Fatal("timed out waiting for the run to start")
	}

	// Nothing the claim did reaches past this instant, so an occurrence that
	// counts here is one whose lease a heartbeat pushed out.
	awaitRenewedLease(t, queue, time.Now().Add(heartbeatLease))
}

// awaitRenewedLease waits for the occurrence to count as in flight at an
// instant the original lease could never cover, and fails when no heartbeat
// ever gets it there.
func awaitRenewedLease(t *testing.T, queue scheduler.WorkQueue, pastClaimLease time.Time) {
	t.Helper()

	ticker := time.NewTicker(heartbeatPoll)
	defer ticker.Stop()

	giveUp := time.After(heartbeatPatience)

	for {
		pending, err := queue.OccurrencePending(t.Context(), heartbeatSchedule, pastClaimLease)
		if err != nil {
			t.Fatalf("occurrence pending: %v", err)
		}

		if pending {
			return
		}

		select {
		case <-ticker.C:
		case <-giveUp:
			t.Fatal("the run's lease was never renewed past the deadline its claim set")
		}
	}
}
