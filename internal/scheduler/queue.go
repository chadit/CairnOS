package scheduler

import (
	"context"
	"time"
)

// DedupKeyFor builds the exactly-once enqueue key for one scheduled occurrence:
// a single work item per (schedule, fire-time). Every node's cron may observe
// the same occurrence, but the unique key lands one row, so the queue needs no
// scheduling leader on the enqueue side (.agents/spec/14-open-decisions.md, S8).
func DedupKeyFor(scheduleID string, fireTime time.Time) string {
	return scheduleID + "|" + fireTime.UTC().Format(time.RFC3339Nano)
}

// pollDedupPrefix tags a polled event's dedup key so it can never collide with a
// scheduled occurrence's (schedule, fire-time) key.
const pollDedupPrefix = "poll"

// PollDedupKeyFor builds the exactly-once enqueue key for one polled event:
// poll|<scheduleID>|<eventID>. A repeat observation of the same event across
// overlapping poll windows or concurrent leader fires lands one row, so a poll
// workflow runs once per event while that row exists
// (.agents/spec/parity/triggers-webhooks.md, "Delivery semantics").
func PollDedupKeyFor(scheduleID, eventID string) string {
	return pollDedupPrefix + "|" + scheduleID + "|" + eventID
}

// WorkState is a queued item's lifecycle state. A cron fire enqueues a ready
// item; a worker leases it; success acks (deletes) it; failure naks it back to
// ready with backoff, or dead-letters it once it has been delivered too many
// times. A held item is parked on a NeedsReauth pause (D12) and is not
// redelivered until resumed (.agents/spec/14-open-decisions.md, S8).
type WorkState string

// Work item states. Completed items are deleted on ack rather than kept in a
// "done" state, so the queue stays a work-queue and not a growing log.
const (
	WorkReady  WorkState = "ready"
	WorkLeased WorkState = "leased"
	WorkHeld   WorkState = "held"
	WorkDead   WorkState = "dead"
)

// WorkItem is one unit of queued work. DedupKey makes enqueue exactly-once
// across nodes: every node's cron may fire for the same (schedule, fire-time),
// but the unique key lands one row. Payload is the opaque JSON a worker needs to
// run the job, kept out of the queue's own concern so the queue does not couple
// to the workflow shape.
type WorkItem struct {
	ID          string
	DedupKey    string
	Payload     []byte
	State       WorkState
	AvailableAt time.Time
	LeaseUntil  time.Time
	Deliveries  int
	CreatedAt   time.Time

	// HoldKey records what a held item is waiting on, set when the item is held
	// and cleared on resume. The queue treats it as opaque; the scheduler stores
	// the re-auth provider name here so a resume probe knows which credential to
	// check (D12). It is empty for any item that is not held.
	HoldKey string
}

// QueueStats is the dashboard snapshot the Grafana panels read: how deep the
// queue is, how much is in flight, and how stale the oldest waiting item is
// (.agents/spec/06-observability-and-tuning.md). OldestReadyAge is zero when
// nothing is waiting.
type QueueStats struct {
	Ready          int
	Leased         int
	Held           int
	Dead           int
	OldestReadyAge time.Duration
}

// WorkQueue is the durable work-queue behind the scheduler. The SQLite backend
// is single-node (its write lock serializes the claim); the Postgres backend
// claims with FOR UPDATE SKIP LOCKED so several workers across nodes pull
// disjoint items. Both satisfy the same contract, proven by one conformance
// suite (.agents/spec/14-open-decisions.md, S8).
type WorkQueue interface {
	// Enqueue inserts a ready item. It is exactly-once on DedupKey: a duplicate
	// key is a no-op that reports inserted=false, so a cron fire that several
	// nodes observe lands a single row.
	Enqueue(ctx context.Context, item *WorkItem) (inserted bool, err error)

	// Claim leases up to limit ready items whose AvailableAt has passed, moving
	// them to leased with LeaseUntil = now + ackWait and incrementing Deliveries.
	// An idle queue returns an empty slice, not an error.
	Claim(ctx context.Context, now time.Time, ackWait time.Duration, limit int) ([]WorkItem, error)

	// Ack removes a finished item. Missing rows are not an error: a sweep may
	// have already reclaimed a lease the worker thought it still held.
	Ack(ctx context.Context, id string) error

	// Nak returns a failed item to ready with AvailableAt = now + backoff, unless
	// Deliveries has reached maxDeliveries, in which case it dead-letters.
	// Reports the resulting state.
	Nak(ctx context.Context, id string, now time.Time, backoff time.Duration, maxDeliveries int) (WorkState, error)

	// Hold parks an item in the held state (a NeedsReauth pause), so the sweep
	// does not redeliver it until Resume returns it to ready. holdKey records what
	// the item waits on (the re-auth provider) for the resume probe; "" is allowed.
	Hold(ctx context.Context, id, holdKey string) error

	// Resume returns a held item to ready, available immediately, and clears its
	// hold key.
	Resume(ctx context.Context, id string) error

	// ListHeld returns every held item with its hold key, so a resume probe can
	// check the credential each one is waiting on (D12). An empty queue or no held
	// items returns an empty slice, not an error.
	ListHeld(ctx context.Context) ([]WorkItem, error)

	// SweepExpired reclaims leases whose LeaseUntil has passed (a crashed or
	// stalled worker): each goes back to ready, or dead-letters once it has hit
	// maxDeliveries. Reports how many were reclaimed and how many dead-lettered.
	SweepExpired(ctx context.Context, now time.Time, maxDeliveries int) (reclaimed, dead int, err error)

	// PurgeDead deletes dead items created before cutoff (the max_age retention
	// sweep), so an inspectable dead-letter set does not grow without bound.
	PurgeDead(ctx context.Context, cutoff time.Time) (int, error)

	// Stats returns the queue-depth snapshot for the dashboard.
	Stats(ctx context.Context, now time.Time) (QueueStats, error)
}

// Compile-time proof both backends satisfy the queue contract, so a signature
// drift surfaces here rather than at the wiring call site.
var (
	_ WorkQueue = (*SQLiteQueue)(nil)
	_ WorkQueue = (*PostgresQueue)(nil)
)
