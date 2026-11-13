// Package schedulertest holds the work-queue conformance suite shared by the
// scheduler's backends. The sqlite unit test and the postgres integration test
// both drive it, so the two backends are held to one behavioral contract.
package schedulertest

import (
	"errors"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/scheduler"
)

const (
	// holdKey is the provider a held item waits on in the hold/resume case, the
	// value Hold records and Resume clears.
	holdKey = "codex"

	// claimLimit is the prefetch cap the cases hand to Claim, large enough that
	// every ready item in a case returns in one call.
	claimLimit = 10
	// maxDeliveries is the dead-letter threshold the retry and sweep cases drive
	// toward.
	maxDeliveries = 3
	// pairCount is the two-item expectation the multi-enqueue cases assert.
	pairCount = 2
	// secondDelivery is the delivery count after exactly one redelivery.
	secondDelivery = 2

	// pastLease advances the clock past the one-minute lease or backoff so a
	// redelivery or sweep is due.
	pastLease = 2 * time.Minute
	// renewedLease is where a heartbeat pushes a lease to, far enough past
	// pastLease that the renewal, not the clock, is what keeps the item alive.
	renewedLease = 10 * time.Minute
	// pastFuture clears a far-future availability so a delayed item is claimable.
	pastFuture = 2 * time.Hour
	// agedReady is how long the oldest-ready-age case lets a ready item sit.
	agedReady = 5 * time.Minute
)

// pendingScheduleID is the schedule the pending-occurrence case asks about.
// Other keys in that case are built around it so a prefix that overlaps is
// exercised rather than assumed away.
const pendingScheduleID = "sched-1"

// queueBaseTime is the fixed clock the conformance subtests reason from. Every
// queue method takes an explicit now, so the suite drives lease and backoff
// decisions without sleeping.
func queueBaseTime() time.Time {
	return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
}

// Queue is the handle a backend gives the suite: the work-queue contract plus
// the poll cursors every backend keeps beside its work-item table.
type Queue interface {
	scheduler.WorkQueue
	scheduler.PollCursors
}

// RunWorkQueueConformance drives one backend through the queue contract. Each
// case gets a fresh, empty queue from newQueue, so the cases stay independent
// and parallel. Q is the backend's own queue type, so newQueue returns the
// concrete handle it opened.
func RunWorkQueueConformance[Q Queue](t *testing.T, newQueue func(*testing.T) Q) {
	t.Helper()

	cases := []struct {
		run  func(*testing.T, Queue)
		name string
	}{
		{name: "EnqueueIsExactlyOnce", run: testQueueEnqueueExactlyOnce},
		{name: "OccurrencePendingTracksUnfinishedWork", run: testQueueOccurrencePending},
		{name: "OccurrencePendingIgnoresExpiredLease", run: testQueueOccurrencePendingIgnoresExpiredLease},
		{name: "RenewHoldsTheLease", run: testQueueRenewHoldsTheLease},
		{name: "ClaimLeasesDueItems", run: testQueueClaimLeasesDueItems},
		{name: "ClaimSkipsFutureItems", run: testQueueClaimSkipsFutureItems},
		{name: "AckRemovesItem", run: testQueueAckRemovesItem},
		{name: "NakRequeuesWithBackoff", run: testQueueNakRequeuesWithBackoff},
		{name: "NakDeadLettersAtLimit", run: testQueueNakDeadLettersAtLimit},
		{name: "SweepReclaimsExpiredLease", run: testQueueSweepReclaimsExpiredLease},
		{name: "SweepDeadLettersExhaustedLease", run: testQueueSweepDeadLettersExhaustedLease},
		{name: "HoldThenResume", run: testQueueHoldThenResume},
		{name: "PurgeDeadRemovesOldItems", run: testQueuePurgeDeadRemovesOldItems},
		{name: "StatsReportsOldestReadyAge", run: testQueueStatsReportsOldestReadyAge},
		{name: "CursorSetThenGet", run: testCursorSetThenGet},
		{name: "CursorUpsertOverwrites", run: testCursorUpsertOverwrites},
		{name: "CursorMissingReportsNotFound", run: testCursorMissingReportsNotFound},
		{name: "CursorPerScheduleIsolation", run: testCursorPerScheduleIsolation},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			testCase.run(t, newQueue(t))
		})
	}
}

func testQueueEnqueueExactlyOnce(t *testing.T, queue Queue) {
	t.Helper()

	base := queueBaseTime()

	if !mustEnqueue(t, queue, "sched-1|t0", base) {
		t.Fatal("first enqueue should insert")
	}

	dup, err := queue.Enqueue(t.Context(), &scheduler.WorkItem{
		DedupKey: "sched-1|t0", Payload: []byte(`{"w":"y"}`), AvailableAt: base, CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("duplicate enqueue: %v", err)
	}

	if dup {
		t.Error("duplicate dedup_key should not insert a second row")
	}

	if stats := mustStats(t, queue, base); stats.Ready != 1 {
		t.Errorf("ready = %d, want 1", stats.Ready)
	}
}

// testQueueOccurrencePending pins what counts as a schedule's unfinished work:
// its own ready and leased occurrences do, an acked one does not, and neither a
// poll event of the same schedule nor another schedule whose id merely starts
// with the same characters ever does.
func testQueueOccurrencePending(t *testing.T, queue Queue) {
	t.Helper()

	base := queueBaseTime()

	assertPending(t, queue, base, false, "empty queue")

	mustEnqueue(t, queue, scheduler.DedupKeyFor(pendingScheduleID, base), base)
	assertPending(t, queue, base, true, "ready occurrence")

	item := mustClaimOne(t, queue, base)
	assertPending(t, queue, base, true, "leased occurrence")

	if err := queue.Ack(t.Context(), item.ID); err != nil {
		t.Fatalf("ack: %v", err)
	}

	assertPending(t, queue, base, false, "acked occurrence")

	mustEnqueue(t, queue, scheduler.PollDedupKeyFor(pendingScheduleID, "42"), base)
	mustEnqueue(t, queue, scheduler.DedupKeyFor(pendingScheduleID+"0", base), base)
	assertPending(t, queue, base, false, "poll event plus a longer schedule id")
}

// testQueueOccurrencePendingIgnoresExpiredLease pins the crash case: a worker
// that dies mid-run leaves its lease behind, and once that lease runs out the
// occurrence stops holding the schedule back. Without this the next fire is
// skipped for the rest of the lease, every time, with nothing to clear it.
func testQueueOccurrencePendingIgnoresExpiredLease(t *testing.T, queue Queue) {
	t.Helper()

	base := queueBaseTime()

	mustEnqueue(t, queue, scheduler.DedupKeyFor(pendingScheduleID, base), base)
	mustClaimOne(t, queue, base) // leased with lease_until = base + 1m, then the worker dies

	assertPending(t, queue, base, true, "lease still live")
	assertPending(t, queue, base.Add(pastLease), false, "lease expired with no worker to renew it")
}

// testQueueRenewHoldsTheLease pins the other half: a worker that is still
// running its item keeps the occurrence in flight past the original deadline,
// and the sweep leaves a renewed lease alone.
func testQueueRenewHoldsTheLease(t *testing.T, queue Queue) {
	t.Helper()

	base := queueBaseTime()

	mustEnqueue(t, queue, scheduler.DedupKeyFor(pendingScheduleID, base), base)
	claimed := mustClaimOne(t, queue, base)

	if err := queue.Renew(t.Context(), []string{claimed.ID}, base.Add(renewedLease)); err != nil {
		t.Fatalf("renew: %v", err)
	}

	assertPending(t, queue, base.Add(pastLease), true, "renewed lease")

	reclaimed, dead, err := queue.SweepExpired(t.Context(), base.Add(pastLease), maxDeliveries)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if reclaimed != 0 || dead != 0 {
		t.Fatalf("sweep reclaimed=%d dead=%d, want 0 and 0 for a renewed lease", reclaimed, dead)
	}
}

func testQueueClaimLeasesDueItems(t *testing.T, queue Queue) {
	t.Helper()

	base := queueBaseTime()

	mustEnqueue(t, queue, "a", base)
	mustEnqueue(t, queue, "b", base)

	claimed, err := queue.Claim(t.Context(), base, time.Minute, claimLimit)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if len(claimed) != pairCount {
		t.Fatalf("claimed %d, want 2", len(claimed))
	}

	for idx := range claimed {
		if claimed[idx].State != scheduler.WorkLeased || claimed[idx].Deliveries != 1 {
			t.Errorf("claimed item = %+v, want leased with 1 delivery", claimed[idx])
		}
	}

	again, err := queue.Claim(t.Context(), base, time.Minute, claimLimit)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}

	if len(again) != 0 {
		t.Errorf("second claim got %d, want 0 (all leased)", len(again))
	}
}

func testQueueClaimSkipsFutureItems(t *testing.T, queue Queue) {
	t.Helper()

	base := queueBaseTime()

	mustEnqueue(t, queue, "later", base.Add(time.Hour))

	now, err := queue.Claim(t.Context(), base, time.Minute, claimLimit)
	if err != nil {
		t.Fatalf("claim now: %v", err)
	}

	if len(now) != 0 {
		t.Errorf("claim before available_at got %d, want 0", len(now))
	}

	future, err := queue.Claim(t.Context(), base.Add(pastFuture), time.Minute, claimLimit)
	if err != nil {
		t.Fatalf("claim future: %v", err)
	}

	if len(future) != 1 {
		t.Errorf("claim after available_at got %d, want 1", len(future))
	}
}

func testQueueAckRemovesItem(t *testing.T, queue Queue) {
	t.Helper()

	base := queueBaseTime()

	mustEnqueue(t, queue, "ack-me", base)
	claimed := mustClaimOne(t, queue, base)

	if err := queue.Ack(t.Context(), claimed.ID); err != nil {
		t.Fatalf("ack: %v", err)
	}

	stats := mustStats(t, queue, base)
	if stats.Ready+stats.Leased+stats.Held+stats.Dead != 0 {
		t.Errorf("after ack stats = %+v, want empty", stats)
	}
}

func testQueueNakRequeuesWithBackoff(t *testing.T, queue Queue) {
	t.Helper()

	base := queueBaseTime()

	mustEnqueue(t, queue, "retry", base)
	claimed := mustClaimOne(t, queue, base)

	state, err := queue.Nak(t.Context(), claimed.ID, base, time.Minute, maxDeliveries)
	if err != nil {
		t.Fatalf("nak: %v", err)
	}

	if state != scheduler.WorkReady {
		t.Fatalf("nak state = %q, want ready", state)
	}

	early, err := queue.Claim(t.Context(), base, time.Minute, claimLimit)
	if err != nil {
		t.Fatalf("claim during backoff: %v", err)
	}

	if len(early) != 0 {
		t.Errorf("claim during backoff got %d, want 0", len(early))
	}

	redelivered := mustClaimOne(t, queue, base.Add(pastLease))
	if redelivered.Deliveries != secondDelivery {
		t.Errorf("redelivered deliveries = %d, want 2", redelivered.Deliveries)
	}
}

func testQueueNakDeadLettersAtLimit(t *testing.T, queue Queue) {
	t.Helper()

	base := queueBaseTime()

	mustEnqueue(t, queue, "doomed", base)
	claimed := mustClaimOne(t, queue, base) // deliveries -> 1

	state, err := queue.Nak(t.Context(), claimed.ID, base, time.Minute, 1)
	if err != nil {
		t.Fatalf("nak: %v", err)
	}

	if state != scheduler.WorkDead {
		t.Fatalf("nak at limit state = %q, want dead", state)
	}

	if stats := mustStats(t, queue, base); stats.Dead != 1 {
		t.Errorf("dead = %d, want 1", stats.Dead)
	}
}

func testQueueSweepReclaimsExpiredLease(t *testing.T, queue Queue) {
	t.Helper()

	base := queueBaseTime()

	mustEnqueue(t, queue, "stalled", base)
	mustClaimOne(t, queue, base) // leased with lease_until = base + 1m

	reclaimed, dead, err := queue.SweepExpired(t.Context(), base.Add(pastLease), maxDeliveries)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if reclaimed != 1 || dead != 0 {
		t.Fatalf("sweep reclaimed=%d dead=%d, want 1 and 0", reclaimed, dead)
	}

	again := mustClaimOne(t, queue, base.Add(pastLease))
	if again.Deliveries != secondDelivery {
		t.Errorf("reclaimed item redelivered with deliveries = %d, want 2", again.Deliveries)
	}
}

func testQueueSweepDeadLettersExhaustedLease(t *testing.T, queue Queue) {
	t.Helper()

	base := queueBaseTime()

	mustEnqueue(t, queue, "exhausted", base)
	mustClaimOne(t, queue, base) // deliveries -> 1, lease_until = base + 1m

	reclaimed, dead, err := queue.SweepExpired(t.Context(), base.Add(pastLease), 1)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if reclaimed != 0 || dead != 1 {
		t.Fatalf("sweep reclaimed=%d dead=%d, want 0 and 1", reclaimed, dead)
	}
}

func testQueueHoldThenResume(t *testing.T, queue Queue) {
	t.Helper()

	base := queueBaseTime()

	mustEnqueue(t, queue, "paused", base)
	claimed := mustClaimOne(t, queue, base)

	if err := queue.Hold(t.Context(), claimed.ID, holdKey); err != nil {
		t.Fatalf("hold: %v", err)
	}

	held, err := queue.Claim(t.Context(), base, time.Minute, claimLimit)
	if err != nil {
		t.Fatalf("claim while held: %v", err)
	}

	if len(held) != 0 {
		t.Errorf("held item should not be claimable; got %d", len(held))
	}

	// ListHeld surfaces the parked item with the provider it waits on, so a
	// resume probe knows which credential to check.
	listed, err := queue.ListHeld(t.Context())
	if err != nil {
		t.Fatalf("list held: %v", err)
	}

	if len(listed) != 1 || listed[0].ID != claimed.ID || listed[0].HoldKey != holdKey {
		t.Fatalf("ListHeld = %+v, want one held item with hold key codex", listed)
	}

	if innerErr := queue.Resume(t.Context(), claimed.ID); innerErr != nil {
		t.Fatalf("resume: %v", innerErr)
	}

	// Resume clears the hold key and empties the held set.
	cleared, err := queue.ListHeld(t.Context())
	if err != nil {
		t.Fatalf("list held after resume: %v", err)
	}

	if len(cleared) != 0 {
		t.Errorf("ListHeld after resume = %+v, want empty", cleared)
	}

	resumed := mustClaimOne(t, queue, base)
	if resumed.ID != claimed.ID {
		t.Errorf("resumed claim id = %q, want %q", resumed.ID, claimed.ID)
	}

	if resumed.HoldKey != "" {
		t.Errorf("resumed item hold key = %q, want empty", resumed.HoldKey)
	}
}

func testQueuePurgeDeadRemovesOldItems(t *testing.T, queue Queue) {
	t.Helper()

	base := queueBaseTime()

	mustEnqueue(t, queue, "trash", base)
	claimed := mustClaimOne(t, queue, base)

	if _, err := queue.Nak(t.Context(), claimed.ID, base, time.Minute, 1); err != nil {
		t.Fatalf("nak to dead: %v", err)
	}

	purged, err := queue.PurgeDead(t.Context(), base.Add(time.Hour))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}

	if purged != 1 {
		t.Errorf("purged %d, want 1", purged)
	}
}

func testQueueStatsReportsOldestReadyAge(t *testing.T, queue Queue) {
	t.Helper()

	base := queueBaseTime()

	mustEnqueue(t, queue, "waiting", base)

	stats := mustStats(t, queue, base.Add(agedReady))
	if stats.Ready != 1 {
		t.Fatalf("ready = %d, want 1", stats.Ready)
	}

	if stats.OldestReadyAge != agedReady {
		t.Errorf("oldest ready age = %v, want 5m", stats.OldestReadyAge)
	}
}

// Poll-cursor case fixtures. Two schedule ids prove per-schedule isolation, and
// two cursor payloads prove an upsert overwrites the prior value.
const (
	cursorScheduleA   = "poll-sched-a"
	cursorScheduleB   = "poll-sched-b"
	cursorValueFirst  = "2026-06-13T12:00:00Z"
	cursorValueSecond = "2026-06-13T12:05:00Z"
)

func testCursorSetThenGet(t *testing.T, cursors Queue) {
	t.Helper()

	if err := cursors.SetCursor(t.Context(), cursorScheduleA, cursorValueFirst); err != nil {
		t.Fatalf("set cursor: %v", err)
	}

	got, err := cursors.GetCursor(t.Context(), cursorScheduleA)
	if err != nil {
		t.Fatalf("get cursor: %v", err)
	}

	if got != cursorValueFirst {
		t.Errorf("cursor = %q, want %q", got, cursorValueFirst)
	}
}

func testCursorUpsertOverwrites(t *testing.T, cursors Queue) {
	t.Helper()

	if err := cursors.SetCursor(t.Context(), cursorScheduleA, cursorValueFirst); err != nil {
		t.Fatalf("first set: %v", err)
	}

	if err := cursors.SetCursor(t.Context(), cursorScheduleA, cursorValueSecond); err != nil {
		t.Fatalf("second set: %v", err)
	}

	got, err := cursors.GetCursor(t.Context(), cursorScheduleA)
	if err != nil {
		t.Fatalf("get cursor: %v", err)
	}

	if got != cursorValueSecond {
		t.Errorf("upserted cursor = %q, want %q", got, cursorValueSecond)
	}
}

func testCursorMissingReportsNotFound(t *testing.T, cursors Queue) {
	t.Helper()

	if _, err := cursors.GetCursor(t.Context(), cursorScheduleA); !errors.Is(err, scheduler.ErrCursorNotFound) {
		t.Fatalf("get missing cursor err = %v, want ErrCursorNotFound", err)
	}
}

func testCursorPerScheduleIsolation(t *testing.T, cursors Queue) {
	t.Helper()

	if err := cursors.SetCursor(t.Context(), cursorScheduleA, cursorValueFirst); err != nil {
		t.Fatalf("set A: %v", err)
	}

	if err := cursors.SetCursor(t.Context(), cursorScheduleB, cursorValueSecond); err != nil {
		t.Fatalf("set B: %v", err)
	}

	gotA, err := cursors.GetCursor(t.Context(), cursorScheduleA)
	if err != nil {
		t.Fatalf("get A: %v", err)
	}

	if gotA != cursorValueFirst {
		t.Errorf("schedule A cursor = %q, want %q", gotA, cursorValueFirst)
	}

	gotB, err := cursors.GetCursor(t.Context(), cursorScheduleB)
	if err != nil {
		t.Fatalf("get B: %v", err)
	}

	if gotB != cursorValueSecond {
		t.Errorf("schedule B cursor = %q, want %q", gotB, cursorValueSecond)
	}
}

// mustEnqueue enqueues a ready item keyed by dedup, failing the test on error.
func mustEnqueue(t *testing.T, queue scheduler.ProducerQueue, dedup string, availableAt time.Time) bool {
	t.Helper()

	inserted, err := queue.Enqueue(t.Context(), &scheduler.WorkItem{
		DedupKey:    dedup,
		Payload:     []byte(`{"workflow":"x"}`),
		AvailableAt: availableAt,
		CreatedAt:   queueBaseTime(),
	})
	if err != nil {
		t.Fatalf("enqueue %s: %v", dedup, err)
	}

	return inserted
}

// assertPending reads the pending-occurrence answer for the schedule under test
// at now and fails when it differs from want; stage names the queue state it is
// read in.
func assertPending(t *testing.T, queue scheduler.OccurrenceQueue, now time.Time, want bool, stage string) {
	t.Helper()

	got, err := queue.OccurrencePending(t.Context(), pendingScheduleID, now)
	if err != nil {
		t.Fatalf("occurrence pending (%s): %v", stage, err)
	}

	if got != want {
		t.Errorf("occurrence pending (%s) = %t, want %t", stage, got, want)
	}
}

// mustClaimOne claims exactly one item, failing when the queue yields a
// different count.
func mustClaimOne(t *testing.T, queue scheduler.LeaseQueue, now time.Time) scheduler.WorkItem {
	t.Helper()

	claimed, err := queue.Claim(t.Context(), now, time.Minute, claimLimit)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if len(claimed) != 1 {
		t.Fatalf("claimed %d items, want 1", len(claimed))
	}

	return claimed[0]
}

// mustStats reads the queue snapshot, failing the test on error.
func mustStats(t *testing.T, queue scheduler.SweepQueue, now time.Time) scheduler.QueueStats {
	t.Helper()

	stats, err := queue.Stats(t.Context(), now)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	return stats
}
