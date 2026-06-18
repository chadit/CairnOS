package scheduler_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/scheduler"
)

// testHoldKey is the provider a held item waits on in the queue and worker
// tests, the value Hold records and Resume clears (D12).
const testHoldKey = "codex"

// queueBaseTime is the fixed clock the conformance subtests reason from. Every
// queue method takes an explicit now, so the suite drives lease and backoff
// decisions without sleeping.
func queueBaseTime() time.Time {
	return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
}

func TestSQLiteQueueConformance(t *testing.T) {
	t.Parallel()

	runWorkQueueConformance(t, func(t *testing.T) scheduler.WorkQueue {
		t.Helper()

		queue, err := scheduler.OpenSQLiteQueue(filepath.Join(t.TempDir(), "queue.db"))
		if err != nil {
			t.Fatalf("open sqlite queue: %v", err)
		}

		t.Cleanup(func() { _ = queue.Close() })

		return queue
	})
}

func TestPostgresQueueConformance(t *testing.T) {
	t.Parallel()

	runWorkQueueConformance(t, func(t *testing.T) scheduler.WorkQueue {
		t.Helper()

		pool := openPool(t) // skips when CAIRNOS_TEST_POSTGRES_DSN is unset

		queue, err := scheduler.NewPostgresQueue(t.Context(), pool)
		if err != nil {
			t.Fatalf("new postgres queue: %v", err)
		}

		if _, err := pool.Exec(t.Context(), "TRUNCATE work_item"); err != nil {
			t.Fatalf("truncate work_item: %v", err)
		}

		return queue
	})
}

// runWorkQueueConformance drives one backend through the queue contract. Each
// case gets a fresh, empty queue from newQueue, so the cases stay independent
// and parallel.
func runWorkQueueConformance(t *testing.T, newQueue func(*testing.T) scheduler.WorkQueue) {
	t.Helper()

	cases := []struct {
		name string
		run  func(*testing.T, scheduler.WorkQueue)
	}{
		{"EnqueueIsExactlyOnce", testQueueEnqueueExactlyOnce},
		{"ClaimLeasesDueItems", testQueueClaimLeasesDueItems},
		{"ClaimSkipsFutureItems", testQueueClaimSkipsFutureItems},
		{"AckRemovesItem", testQueueAckRemovesItem},
		{"NakRequeuesWithBackoff", testQueueNakRequeuesWithBackoff},
		{"NakDeadLettersAtLimit", testQueueNakDeadLettersAtLimit},
		{"SweepReclaimsExpiredLease", testQueueSweepReclaimsExpiredLease},
		{"SweepDeadLettersExhaustedLease", testQueueSweepDeadLettersExhaustedLease},
		{"HoldThenResume", testQueueHoldThenResume},
		{"PurgeDeadRemovesOldItems", testQueuePurgeDeadRemovesOldItems},
		{"StatsReportsOldestReadyAge", testQueueStatsReportsOldestReadyAge},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			testCase.run(t, newQueue(t))
		})
	}
}

func testQueueEnqueueExactlyOnce(t *testing.T, queue scheduler.WorkQueue) {
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

func testQueueClaimLeasesDueItems(t *testing.T, queue scheduler.WorkQueue) {
	t.Helper()

	base := queueBaseTime()

	mustEnqueue(t, queue, "a", base)
	mustEnqueue(t, queue, "b", base)

	claimed, err := queue.Claim(t.Context(), base, time.Minute, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if len(claimed) != 2 {
		t.Fatalf("claimed %d, want 2", len(claimed))
	}

	for idx := range claimed {
		if claimed[idx].State != scheduler.WorkLeased || claimed[idx].Deliveries != 1 {
			t.Errorf("claimed item = %+v, want leased with 1 delivery", claimed[idx])
		}
	}

	again, err := queue.Claim(t.Context(), base, time.Minute, 10)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}

	if len(again) != 0 {
		t.Errorf("second claim got %d, want 0 (all leased)", len(again))
	}
}

func testQueueClaimSkipsFutureItems(t *testing.T, queue scheduler.WorkQueue) {
	t.Helper()

	base := queueBaseTime()

	mustEnqueue(t, queue, "later", base.Add(time.Hour))

	now, err := queue.Claim(t.Context(), base, time.Minute, 10)
	if err != nil {
		t.Fatalf("claim now: %v", err)
	}

	if len(now) != 0 {
		t.Errorf("claim before available_at got %d, want 0", len(now))
	}

	future, err := queue.Claim(t.Context(), base.Add(2*time.Hour), time.Minute, 10)
	if err != nil {
		t.Fatalf("claim future: %v", err)
	}

	if len(future) != 1 {
		t.Errorf("claim after available_at got %d, want 1", len(future))
	}
}

func testQueueAckRemovesItem(t *testing.T, queue scheduler.WorkQueue) {
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

func testQueueNakRequeuesWithBackoff(t *testing.T, queue scheduler.WorkQueue) {
	t.Helper()

	base := queueBaseTime()

	mustEnqueue(t, queue, "retry", base)
	claimed := mustClaimOne(t, queue, base)

	state, err := queue.Nak(t.Context(), claimed.ID, base, time.Minute, 3)
	if err != nil {
		t.Fatalf("nak: %v", err)
	}

	if state != scheduler.WorkReady {
		t.Fatalf("nak state = %q, want ready", state)
	}

	early, err := queue.Claim(t.Context(), base, time.Minute, 10)
	if err != nil {
		t.Fatalf("claim during backoff: %v", err)
	}

	if len(early) != 0 {
		t.Errorf("claim during backoff got %d, want 0", len(early))
	}

	redelivered := mustClaimOne(t, queue, base.Add(2*time.Minute))
	if redelivered.Deliveries != 2 {
		t.Errorf("redelivered deliveries = %d, want 2", redelivered.Deliveries)
	}
}

func testQueueNakDeadLettersAtLimit(t *testing.T, queue scheduler.WorkQueue) {
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

func testQueueSweepReclaimsExpiredLease(t *testing.T, queue scheduler.WorkQueue) {
	t.Helper()

	base := queueBaseTime()

	mustEnqueue(t, queue, "stalled", base)
	mustClaimOne(t, queue, base) // leased with lease_until = base + 1m

	reclaimed, dead, err := queue.SweepExpired(t.Context(), base.Add(2*time.Minute), 3)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if reclaimed != 1 || dead != 0 {
		t.Fatalf("sweep reclaimed=%d dead=%d, want 1 and 0", reclaimed, dead)
	}

	again := mustClaimOne(t, queue, base.Add(2*time.Minute))
	if again.Deliveries != 2 {
		t.Errorf("reclaimed item redelivered with deliveries = %d, want 2", again.Deliveries)
	}
}

func testQueueSweepDeadLettersExhaustedLease(t *testing.T, queue scheduler.WorkQueue) {
	t.Helper()

	base := queueBaseTime()

	mustEnqueue(t, queue, "exhausted", base)
	mustClaimOne(t, queue, base) // deliveries -> 1, lease_until = base + 1m

	reclaimed, dead, err := queue.SweepExpired(t.Context(), base.Add(2*time.Minute), 1)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if reclaimed != 0 || dead != 1 {
		t.Fatalf("sweep reclaimed=%d dead=%d, want 0 and 1", reclaimed, dead)
	}
}

func testQueueHoldThenResume(t *testing.T, queue scheduler.WorkQueue) {
	t.Helper()

	base := queueBaseTime()

	mustEnqueue(t, queue, "paused", base)
	claimed := mustClaimOne(t, queue, base)

	if err := queue.Hold(t.Context(), claimed.ID, testHoldKey); err != nil {
		t.Fatalf("hold: %v", err)
	}

	held, err := queue.Claim(t.Context(), base, time.Minute, 10)
	if err != nil {
		t.Fatalf("claim while held: %v", err)
	}

	if len(held) != 0 {
		t.Errorf("held item should not be claimable; got %d", len(held))
	}

	// ListHeld surfaces the parked item with the provider it waits on, so a
	// resume probe knows which credential to check (D12).
	listed, err := queue.ListHeld(t.Context())
	if err != nil {
		t.Fatalf("list held: %v", err)
	}

	if len(listed) != 1 || listed[0].ID != claimed.ID || listed[0].HoldKey != testHoldKey {
		t.Fatalf("ListHeld = %+v, want one held item with hold key codex", listed)
	}

	if err := queue.Resume(t.Context(), claimed.ID); err != nil {
		t.Fatalf("resume: %v", err)
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

func testQueuePurgeDeadRemovesOldItems(t *testing.T, queue scheduler.WorkQueue) {
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

func testQueueStatsReportsOldestReadyAge(t *testing.T, queue scheduler.WorkQueue) {
	t.Helper()

	base := queueBaseTime()

	mustEnqueue(t, queue, "waiting", base)

	stats := mustStats(t, queue, base.Add(5*time.Minute))
	if stats.Ready != 1 {
		t.Fatalf("ready = %d, want 1", stats.Ready)
	}

	if stats.OldestReadyAge != 5*time.Minute {
		t.Errorf("oldest ready age = %v, want 5m", stats.OldestReadyAge)
	}
}

// mustEnqueue enqueues a ready item keyed by dedup, failing the test on error.
func mustEnqueue(t *testing.T, queue scheduler.WorkQueue, dedup string, availableAt time.Time) bool {
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

// mustClaimOne claims exactly one item, failing when the queue yields a
// different count.
func mustClaimOne(t *testing.T, queue scheduler.WorkQueue, now time.Time) scheduler.WorkItem {
	t.Helper()

	claimed, err := queue.Claim(t.Context(), now, time.Minute, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if len(claimed) != 1 {
		t.Fatalf("claimed %d items, want 1", len(claimed))
	}

	return claimed[0]
}

// mustStats reads the queue snapshot, failing the test on error.
func mustStats(t *testing.T, queue scheduler.WorkQueue, now time.Time) scheduler.QueueStats {
	t.Helper()

	stats, err := queue.Stats(t.Context(), now)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	return stats
}
