package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Worker defaults, used when a WorkerConfig leaves a knob zero. They favor a
// small, calm footprint; an operator tunes them per deployment
// (.agents/spec/14-open-decisions.md, S8).
const (
	defaultConcurrency   = 4
	defaultPrefetch      = 8
	defaultAckWait       = 5 * time.Minute
	defaultBackoff       = 30 * time.Second
	defaultMaxDeliveries = 5
	defaultSweepInterval = time.Minute
	defaultWorkerPoll    = 2 * time.Second
)

// Lease heartbeat. The worker renews three times per lease, so two lost beats
// still leave a live run holding its item. The floor keeps a test's tiny lease
// from handing time.NewTicker a zero period.
const (
	renewsPerLease   = 3
	minRenewInterval = time.Millisecond
)

// QueueRunner executes one claimed work item. The worker acks the item when Run
// returns nil and naks it (backoff or dead-letter) when Run returns an error, so
// a runner reports failure by returning rather than by touching the queue.
type QueueRunner interface {
	Run(ctx context.Context, item *WorkItem) error
}

// ItemMetrics counts how each work item settles.
type ItemMetrics interface {
	// ItemRun records one item finishing, ok=false on a nak.
	ItemRun(ok bool)
	// ItemDeadLettered records an item exhausting its deliveries.
	ItemDeadLettered()
	// ItemHeld records an item parked for re-authentication (D12).
	ItemHeld()
	// ItemResumed records a held item returned to ready after its credential
	// became valid again (D12).
	ItemResumed()
}

// SweepMetrics receives what the periodic sweep observes across the queue.
type SweepMetrics interface {
	// LeaseReclaimed records a crashed-worker lease the sweep returned to ready.
	LeaseReclaimed(count int)
	// Depth reports the queue-depth snapshot after a sweep.
	Depth(stats QueueStats)
}

// QueueMetrics receives the worker's counters so a telemetry adapter can export
// them to the dashboard (.agents/spec/06-observability-and-tuning.md). A nil
// metrics value disables reporting; the worker never calls a nil.
type QueueMetrics interface {
	ItemMetrics
	SweepMetrics
}

// WorkerConfig wires one Worker. Queue and Runner are required; every duration
// and count falls back to a default when left zero.
type WorkerConfig struct {
	Runner  QueueRunner
	Queue   WorkQueue
	Metrics QueueMetrics
	Clock   func() time.Time

	// ResumeProbe reports whether a held item's credential (named by its hold
	// key) is valid again at now, so the sweep can resume it without a human. The
	// CLI wires it to the reauth credential probe; nil disables auto-resume, so
	// held items wait for a manual resume, keeping the scheduler package free of
	// the reauth dependency (D12).
	ResumeProbe func(holdKey string, now time.Time) bool

	// OnDeadLetter runs after an item exhausts its deliveries and settles
	// WorkDead, so the CLI can fan a dead_lettered line out to Discord. It fires
	// only on the terminal dead-letter, never on an intermediate requeue. nil
	// disables the notification; the item is still logged and counted the same
	// way (.agents/spec/parity/messaging-bridge.md, decision 7).
	OnDeadLetter func(ctx context.Context, item *WorkItem, runErr error)

	// OnHold runs after a job is parked for re-auth, so the CLI can alert an
	// operator (a Discord webhook) with the tool and the remediation. nil
	// disables notification; the scheduler package stays free of the notify and
	// runner dependencies, the same seam as Holdable (D12).
	OnHold func(ctx context.Context, item *WorkItem, runErr error)

	// HoldKey derives what a held item is waiting on (the re-auth provider) from
	// the run error, stored on the item so a resume probe knows which credential
	// to check. The scheduler does not interpret it (D12). A nil HoldKey, or one
	// returning "", means the error cannot be held: it settles as a failure.
	HoldKey func(error) string

	// Holdable reports whether a run error means the job should be held for a
	// human (an expired credential) rather than redelivered or dead-lettered.
	// The CLI wires it to runner.IsReauth; nil means no error ever holds, so the
	// scheduler package stays free of the runner dependency (D12).
	Holdable func(error) bool

	Logger        *slog.Logger
	AckWait       time.Duration
	PollInterval  time.Duration
	MaxAge        time.Duration
	SweepInterval time.Duration
	MaxDeliveries int
	Backoff       time.Duration
	Prefetch      int
	Concurrency   int
}

// Worker drains the queue: it claims ready items up to its free capacity, runs
// each through the QueueRunner, and acks or naks the result. A separate sweep
// loop reclaims leases abandoned by crashed workers and dead-letters items past
// their delivery limit, so no fire is silently lost
// (.agents/spec/14-open-decisions.md, S8).
type Worker struct {
	runner        QueueRunner
	queue         WorkQueue
	metrics       QueueMetrics
	clock         func() time.Time
	resumeProbe   func(holdKey string, now time.Time) bool
	onDeadLetter  func(ctx context.Context, item *WorkItem, runErr error)
	onHold        func(ctx context.Context, item *WorkItem, runErr error)
	holdKey       func(error) string
	holdable      func(error) bool
	logger        *slog.Logger
	leases        *leaseSet
	ackWait       time.Duration
	pollInterval  time.Duration
	maxAge        time.Duration
	sweepInterval time.Duration
	maxDeliveries int
	backoff       time.Duration
	prefetch      int
	concurrency   int
}

// leaseSet is the ids this worker is running right now, which is what the
// heartbeat renews. Every method is safe from any goroutine.
type leaseSet struct {
	ids map[string]struct{}
	mu  sync.Mutex
}

// add records an item as running under this worker's lease.
func (s *leaseSet) add(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ids[id] = struct{}{}
}

// remove drops a settled item so its lease stops being renewed.
func (s *leaseSet) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.ids, id)
}

// snapshot copies the running ids, so the renewal query runs outside the lock.
func (s *leaseSet) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]string, 0, len(s.ids))
	for id := range s.ids {
		ids = append(ids, id)
	}

	return ids
}

// NewWorker builds a Worker, filling WorkerConfig defaults.
func NewWorker(cfg *WorkerConfig) *Worker {
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Worker{
		queue:         cfg.Queue,
		runner:        cfg.Runner,
		concurrency:   orDefault(cfg.Concurrency, defaultConcurrency),
		prefetch:      orDefault(cfg.Prefetch, defaultPrefetch),
		ackWait:       orDefaultDuration(cfg.AckWait, defaultAckWait),
		backoff:       orDefaultDuration(cfg.Backoff, defaultBackoff),
		maxDeliveries: orDefault(cfg.MaxDeliveries, defaultMaxDeliveries),
		sweepInterval: orDefaultDuration(cfg.SweepInterval, defaultSweepInterval),
		maxAge:        cfg.MaxAge, // zero means never purge dead items
		pollInterval:  orDefaultDuration(cfg.PollInterval, defaultWorkerPoll),
		clock:         clock,
		logger:        logger,
		leases:        &leaseSet{ids: map[string]struct{}{}},
		metrics:       cfg.Metrics,
		holdable:      cfg.Holdable,
		holdKey:       cfg.HoldKey,
		onHold:        cfg.OnHold,
		onDeadLetter:  cfg.OnDeadLetter,
		resumeProbe:   cfg.ResumeProbe,
	}
}

// Run drives the worker until ctx is canceled, which is the clean stop (Run
// returns nil). It runs the sweep loop alongside the claim loop and waits for
// both, plus any in-flight items, before returning.
func (w *Worker) Run(ctx context.Context) error {
	if w.queue == nil {
		return ErrNoStore
	}

	if w.runner == nil {
		return ErrNoRunner
	}

	var wg sync.WaitGroup

	wg.Go(func() {
		w.sweepLoop(ctx)
	})

	wg.Go(func() {
		w.renewLoop(ctx)
	})

	w.claimLoop(ctx)
	wg.Wait()

	return nil
}

// claimLoop claims and dispatches items until ctx cancels, then waits for the
// in-flight items it launched. It claims at most its free capacity each pass, so
// it never leases more than it can promptly run.
func (w *Worker) claimLoop(ctx context.Context) {
	sem := make(chan struct{}, w.concurrency)

	var inflight sync.WaitGroup

	defer inflight.Wait()

	for {
		claimed := w.claimBatch(ctx, sem)

		for idx := range claimed {
			item := claimed[idx]

			sem <- struct{}{}

			inflight.Go(func() {
				defer func() { <-sem }()

				w.process(ctx, &item)
			})
		}

		// An empty claim means the queue is idle: sleep before polling again. A
		// non-empty claim loops straight back to drain the backlog quickly.
		if len(claimed) == 0 && !sleepCtx(ctx, w.pollInterval) {
			return
		}

		if ctx.Err() != nil {
			return
		}
	}
}

// claimBatch claims up to the worker's free capacity, capped by prefetch. A
// claim error is logged and treated as an empty batch, so a transient store
// failure pauses the worker instead of killing it. sem is receive-only here: the
// batch only reads its length to size the claim, never sends.
func (w *Worker) claimBatch(ctx context.Context, sem <-chan struct{}) []WorkItem {
	free := w.concurrency - len(sem)
	if free <= 0 {
		return nil
	}

	claimed, err := w.queue.Claim(ctx, w.clock(), w.ackWait, min(w.prefetch, free))
	if err != nil {
		if ctx.Err() == nil {
			w.logger.Error("worker claim", "error", err)
		}

		return nil
	}

	return claimed
}

// process runs one item and settles it: ack on success, nak on failure. The
// settle uses a detached, short context so it still records the outcome when the
// worker is shutting down and ctx is already canceled.
func (w *Worker) process(ctx context.Context, item *WorkItem) {
	// Held across the settle, not just the run: the item is this worker's until
	// the ack or nak lands, and a lease that lapses in between reads as abandoned.
	w.leases.add(item.ID)
	defer w.leases.remove(item.ID)

	runErr := w.runner.Run(ctx, item)

	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.ackWait)
	defer cancel()

	if runErr == nil {
		if err := w.queue.Ack(settleCtx, item.ID); err != nil {
			w.logger.Error("worker ack", "item", item.ID, "error", err)
		}

		w.report(true)

		return
	}

	if w.holdable != nil && w.holdable(runErr) {
		w.holdItem(settleCtx, item, runErr)

		return
	}

	w.settleFailure(settleCtx, item, runErr)
}

// holdItem parks an item whose run failed on an expired credential, so a human
// can re-authenticate and resume it rather than the queue burning its deliveries
// and dead-lettering a job that was never going to succeed without re-auth (D12,
// .agents/spec/14-open-decisions.md). A failed hold falls back to a normal nak,
// so the item is never silently stuck leased.
func (w *Worker) holdItem(ctx context.Context, item *WorkItem, runErr error) {
	var holdKey string
	if w.holdKey != nil {
		holdKey = w.holdKey(runErr)
	}

	// Nothing can probe a hold that names no credential, so it would sit held
	// forever and keep its schedule's overlap guard shut. Fail it like any other
	// failure instead; the error names the tool that was not recognized.
	if holdKey == "" {
		w.logger.Warn("hold key unresolved, failing work item instead of holding",
			"item", item.ID, "dedup", item.DedupKey, "error", runErr)
		w.settleFailure(ctx, item, runErr)

		return
	}

	if err := w.queue.Hold(ctx, item.ID, holdKey); err != nil {
		w.logger.Error("worker hold", "item", item.ID, "error", err)
		w.settleFailure(ctx, item, runErr)

		return
	}

	w.logger.Warn("work item held",
		"item", item.ID, "dedup", item.DedupKey, "hold_key", holdKey, "error", runErr)

	if w.metrics != nil {
		w.metrics.ItemHeld()
	}

	// Notify last and only after the hold succeeded: a held job is already safe,
	// so a slow or failed alert never changes its fate (D12). onHold runs on the
	// detached settle context, which carries the ack-wait timeout.
	if w.onHold != nil {
		w.onHold(ctx, item, runErr)
	}
}

// settleFailure naks a failed item and logs whether it was requeued or
// dead-lettered, so a deferral is visible on the dashboard and in the log
// (.agents/spec/14-open-decisions.md, S8).
func (w *Worker) settleFailure(ctx context.Context, item *WorkItem, runErr error) {
	state, err := w.queue.Nak(ctx, item.ID, w.clock(), w.backoff, w.maxDeliveries)
	if err != nil {
		w.logger.Error("worker nak", "item", item.ID, "error", err)

		return
	}

	w.report(false)

	if state == WorkDead {
		w.logger.Error("work item dead-lettered after retries",
			"item", item.ID, "dedup", item.DedupKey, "deliveries", item.Deliveries, "error", runErr)

		if w.metrics != nil {
			w.metrics.ItemDeadLettered()
		}

		// Notify last and only on the terminal dead-letter: the item is already
		// settled, so a slow or failed fan-out never changes its fate. Fired on the
		// detached settle context, which carries the ack-wait timeout.
		if w.onDeadLetter != nil {
			w.onDeadLetter(ctx, item, runErr)
		}

		return
	}

	w.logger.Warn("work item deferred for retry",
		"item", item.ID, "dedup", item.DedupKey, "deliveries", item.Deliveries,
		"backoff", w.backoff, "error", runErr)
}

// renewLoop keeps the leases of this worker's in-flight items alive until ctx
// cancels. It is what makes the lease a liveness signal rather than a guess at
// how long a run takes: a long run holds its item, and a worker that dies stops
// beating, so its items fall free on the next deadline.
func (w *Worker) renewLoop(ctx context.Context) {
	ticker := time.NewTicker(w.renewInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.renewOnce(ctx)
		}
	}
}

// renewInterval is the heartbeat period, a share of the lease rather than a
// fixed span, so tuning ack_wait tunes both ends together.
func (w *Worker) renewInterval() time.Duration {
	return max(w.ackWait/renewsPerLease, minRenewInterval)
}

// renewOnce pushes out the leases this worker still holds. A failure is logged
// and left to the next beat, which still lands well inside the lease.
func (w *Worker) renewOnce(ctx context.Context) {
	ids := w.leases.snapshot()
	if len(ids) == 0 {
		return
	}

	if err := w.queue.Renew(ctx, ids, w.clock().Add(w.ackWait)); err != nil {
		w.logger.Error("worker renew leases", "items", len(ids), "error", err)
	}
}

// sweepLoop reclaims expired leases and purges aged dead items on a timer until
// ctx cancels.
func (w *Worker) sweepLoop(ctx context.Context) {
	ticker := time.NewTicker(w.sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.sweepOnce(ctx)
		}
	}
}

// sweepOnce reclaims expired leases, purges aged dead items when MaxAge is set,
// and publishes the depth snapshot.
func (w *Worker) sweepOnce(ctx context.Context) {
	now := w.clock()

	reclaimed, dead, err := w.queue.SweepExpired(ctx, now, w.maxDeliveries)
	if err != nil {
		w.logger.Error("worker sweep", "error", err)

		return
	}

	if reclaimed > 0 || dead > 0 {
		w.logger.Warn("reclaimed expired leases", "reclaimed", reclaimed, "dead_lettered", dead)

		if w.metrics != nil {
			w.metrics.LeaseReclaimed(reclaimed)
		}
	}

	w.resumeHeld(ctx, now)
	w.purgeAged(ctx, now)
	w.publishDepth(ctx, now)
}

// resumeHeld returns held items to ready once their credential is valid again, so
// a re-authenticated tool's jobs flow without a human touching the queue (D12). A
// nil probe disables auto-resume, but the keyless release below still runs: a row
// no probe can name is stuck otherwise.
func (w *Worker) resumeHeld(ctx context.Context, now time.Time) {
	held, err := w.queue.ListHeld(ctx)
	if err != nil {
		w.logger.Error("worker list held", "error", err)

		return
	}

	for idx := range held {
		item := held[idx]

		switch {
		case item.HoldKey == "":
			w.releaseKeyless(ctx, &item)
		case w.resumeProbe != nil && w.resumeProbe(item.HoldKey, now):
			w.resumeItem(ctx, &item)
		}
	}
}

// releaseKeyless returns a keyless held item to ready so a host upgraded past the
// keyless hold heals itself. The retry either succeeds or settles as a failure,
// which the overlap guard reads as finished either way.
func (w *Worker) releaseKeyless(ctx context.Context, item *WorkItem) {
	if err := w.queue.Resume(ctx, item.ID); err != nil {
		w.logger.Error("worker release keyless hold", "item", item.ID, "error", err)

		return
	}

	w.logger.Warn("released held work item that names no credential",
		"item", item.ID, "dedup", item.DedupKey)
}

// resumeItem returns one held item to ready after its probe reported the
// credential valid. A resume error is logged and the item is left for the next
// sweep.
func (w *Worker) resumeItem(ctx context.Context, item *WorkItem) {
	if err := w.queue.Resume(ctx, item.ID); err != nil {
		w.logger.Error("worker resume", "item", item.ID, "error", err)

		return
	}

	w.logger.Info("held work item resumed after re-authentication",
		"item", item.ID, "dedup", item.DedupKey, "provider", item.HoldKey)

	if w.metrics != nil {
		w.metrics.ItemResumed()
	}
}

// purgeAged deletes dead items older than MaxAge, a no-op when MaxAge is unset.
func (w *Worker) purgeAged(ctx context.Context, now time.Time) {
	if w.maxAge <= 0 {
		return
	}

	purged, err := w.queue.PurgeDead(ctx, now.Add(-w.maxAge))
	if err != nil {
		w.logger.Error("worker purge dead", "error", err)

		return
	}

	if purged > 0 {
		w.logger.Info("purged aged dead work items", "purged", purged)
	}
}

// publishDepth reads the queue snapshot and hands it to the metrics sink.
func (w *Worker) publishDepth(ctx context.Context, now time.Time) {
	if w.metrics == nil {
		return
	}

	stats, err := w.queue.Stats(ctx, now)
	if err != nil {
		w.logger.Error("worker stats", "error", err)

		return
	}

	w.metrics.Depth(stats)
}

// report forwards a run outcome to the metrics sink when one is wired.
func (w *Worker) report(ok bool) {
	if w.metrics != nil {
		w.metrics.ItemRun(ok)
	}
}

// orDefault returns value when positive, else fallback.
func orDefault(value, fallback int) int {
	if value > 0 {
		return value
	}

	return fallback
}

// orDefaultDuration returns value when positive, else fallback.
func orDefaultDuration(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}

	return fallback
}
