package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"
)

// pgWorkItemSchema is the Postgres work-queue table (.agents/spec/14-open-decisions.md,
// S8): JSONB payload, timestamptz, a unique dedup_key for exactly-once enqueue,
// and the claim index over (state, available_at) the FOR UPDATE SKIP LOCKED scan
// reads.
const pgWorkItemSchema = `
CREATE TABLE IF NOT EXISTS work_item (
    id           TEXT PRIMARY KEY,
    dedup_key    TEXT NOT NULL UNIQUE,
    payload      JSONB NOT NULL,
    state        TEXT NOT NULL,
    available_at TIMESTAMPTZ NOT NULL,
    lease_until  TIMESTAMPTZ,
    deliveries   INTEGER NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL,
    hold_key     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_work_item_claim ON work_item(state, available_at);
`

// pgWorkItemColumns is the RETURNING / SELECT list every Postgres work-item
// query shares, in scanPGWorkItem order.
const pgWorkItemColumns = "id, dedup_key, payload, state, available_at, lease_until, deliveries, created_at, hold_key"

// PostgresQueue is the multi-node WorkQueue backend. Its claim uses FOR UPDATE
// SKIP LOCKED, so several workers across nodes pull disjoint items without
// blocking each other. It borrows a shared pool and does not own it; the pool's
// creator closes it.
type PostgresQueue struct {
	pool *pgxpool.Pool
}

// NewPostgresQueue applies the work-queue schema on a shared pool and returns
// the backend. It takes the pool rather than a DSN so a node runs one pool for
// the run store, schedules, and the queue together.
func NewPostgresQueue(ctx context.Context, pool *pgxpool.Pool) (*PostgresQueue, error) {
	if pool == nil {
		return nil, ErrNoStore
	}

	if _, err := pool.Exec(ctx, pgWorkItemSchema); err != nil {
		return nil, fmt.Errorf("create work-queue schema: %w", err)
	}

	// The poll-cursor table rides the same pool as the work-item table, so a
	// poll fire persists its cursor through the one queue handle it enqueues on.
	if _, err := pool.Exec(ctx, pgPollCursorSchema); err != nil {
		return nil, fmt.Errorf("create poll-cursor schema: %w", err)
	}

	return &PostgresQueue{pool: pool}, nil
}

// Enqueue inserts a ready item, exactly-once on dedup_key. ON CONFLICT DO
// NOTHING makes a duplicate a no-op, so every node's cron may fire for one
// occurrence and a single row lands.
func (q *PostgresQueue) Enqueue(ctx context.Context, item *WorkItem) (bool, error) {
	id := item.ID
	if id == "" {
		id = ulid.Make().String()
	}

	createdAt := item.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	availableAt := item.AvailableAt
	if availableAt.IsZero() {
		availableAt = createdAt
	}

	tag, err := q.pool.Exec(ctx,
		`INSERT INTO work_item (id, dedup_key, payload, state, available_at, lease_until, deliveries, created_at)
		 VALUES ($1, $2, $3, $4, $5, NULL, 0, $6)
		 ON CONFLICT (dedup_key) DO NOTHING`,
		id, item.DedupKey, item.Payload, string(WorkReady), availableAt.UTC(), createdAt.UTC())
	if err != nil {
		return false, fmt.Errorf("enqueue work item %s: %w", item.DedupKey, err)
	}

	return tag.RowsAffected() == 1, nil
}

// OccurrencePending reports whether the schedule has an unfinished occurrence.
// It matches the dedup-key prefix with substr rather than LIKE so a schedule id
// never needs escaping.
//
// A leased row counts only while its lease is live: a lease nobody renewed
// outlived the worker that took it, so counting it would let one killed process
// block the schedule.
func (q *PostgresQueue) OccurrencePending(ctx context.Context, scheduleID string, now time.Time) (bool, error) {
	prefix := OccurrencePrefix(scheduleID)
	waiting := []string{string(WorkReady), string(WorkHeld)}

	var pending bool

	err := q.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM work_item
		 WHERE substr(dedup_key, 1, length($1)) = $1
		   AND (state = ANY($2) OR (state = $3 AND lease_until > $4)))`,
		prefix, waiting, string(WorkLeased), now.UTC()).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("pending occurrence for schedule %s: %w", scheduleID, err)
	}

	return pending, nil
}

// Claim leases up to limit ready, due items in one statement: the subselect
// takes a row lock with SKIP LOCKED so concurrent claimers never wait on or
// double-take each other's rows, and the outer UPDATE returns the leased rows.
func (q *PostgresQueue) Claim(ctx context.Context, now time.Time, ackWait time.Duration, limit int) ([]WorkItem, error) {
	rows, err := q.pool.Query(ctx,
		`UPDATE work_item SET state = $1, lease_until = $2, deliveries = deliveries + 1
		 WHERE id IN (
		     SELECT id FROM work_item
		     WHERE state = $3 AND available_at <= $4
		     ORDER BY available_at ASC, id ASC
		     FOR UPDATE SKIP LOCKED
		     LIMIT $5
		 )
		 RETURNING `+pgWorkItemColumns,
		string(WorkLeased), now.Add(ackWait).UTC(), string(WorkReady), now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("claim work items: %w", err)
	}

	return scanPGWorkItems(rows)
}

// Renew pushes the lease deadline of the given leased items out, the heartbeat
// a worker sends while its runs are still going. An item that already settled
// no longer matches the leased guard, so a renewal racing a settle quietly does
// nothing.
func (q *PostgresQueue) Renew(ctx context.Context, ids []string, leaseUntil time.Time) error {
	if len(ids) == 0 {
		return nil
	}

	_, err := q.pool.Exec(ctx,
		"UPDATE work_item SET lease_until = $1 WHERE state = $2 AND id = ANY($3)",
		leaseUntil.UTC(), string(WorkLeased), ids)
	if err != nil {
		return fmt.Errorf("renew %d work item leases: %w", len(ids), err)
	}

	return nil
}

// Ack removes a finished item. A missing row is not an error.
func (q *PostgresQueue) Ack(ctx context.Context, id string) error {
	if _, err := q.pool.Exec(ctx, "DELETE FROM work_item WHERE id = $1", id); err != nil {
		return fmt.Errorf("ack work item %s: %w", id, err)
	}

	return nil
}

// Nak returns a failed item to ready with a backoff delay, or dead-letters it
// once Deliveries has reached maxDeliveries. The CASE decides in one statement
// and RETURNING reports the resulting state.
func (q *PostgresQueue) Nak(ctx context.Context, id string, now time.Time, backoff time.Duration, maxDeliveries int) (WorkState, error) {
	var next string

	err := q.pool.QueryRow(ctx,
		`UPDATE work_item
		 SET state = CASE WHEN deliveries >= $1 THEN $2 ELSE $3 END,
		     available_at = $4,
		     lease_until = NULL
		 WHERE id = $5
		 RETURNING state`,
		maxDeliveries, string(WorkDead), string(WorkReady), now.Add(backoff).UTC(), id).Scan(&next)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("%w: %s", ErrWorkItemNotFound, id)
		}

		return "", fmt.Errorf("nak work item %s: %w", id, err)
	}

	return WorkState(next), nil
}

// Hold parks a leased item so the sweep does not redeliver it until Resume,
// recording holdKey (the re-auth provider) so a resume probe knows which
// credential the item is waiting on.
func (q *PostgresQueue) Hold(ctx context.Context, id, holdKey string) error {
	return q.setState(ctx, "hold", id,
		"UPDATE work_item SET state = $1, lease_until = NULL, hold_key = $2 WHERE id = $3",
		string(WorkHeld), holdKey, id)
}

// Resume returns a held item to ready and clears its hold key. It leaves
// available_at alone: a held item was leased, so its available_at already sits in
// the past, which makes the resumed item immediately claimable without coupling
// resume to a clock.
func (q *PostgresQueue) Resume(ctx context.Context, id string) error {
	return q.setState(ctx, "resume", id,
		"UPDATE work_item SET state = $1, lease_until = NULL, hold_key = '' WHERE id = $2 AND state = $3",
		string(WorkReady), id, string(WorkHeld))
}

// ListHeld returns every held item with its hold key, so a resume probe can
// check the credential each one is waiting on (D12).
func (q *PostgresQueue) ListHeld(ctx context.Context) ([]WorkItem, error) {
	rows, err := q.pool.Query(ctx,
		"SELECT "+pgWorkItemColumns+" FROM work_item WHERE state = $1 ORDER BY id ASC", string(WorkHeld))
	if err != nil {
		return nil, fmt.Errorf("list held work items: %w", err)
	}

	return scanPGWorkItems(rows)
}

// SweepExpired reclaims leases past their deadline: those under the delivery
// limit go back to ready, those at it dead-letter.
func (q *PostgresQueue) SweepExpired(ctx context.Context, now time.Time, maxDeliveries int) (int, int, error) {
	nowUTC := now.UTC()

	deadTag, err := q.pool.Exec(ctx,
		"UPDATE work_item SET state = $1, lease_until = NULL WHERE state = $2 AND lease_until <= $3 AND deliveries >= $4",
		string(WorkDead), string(WorkLeased), nowUTC, maxDeliveries)
	if err != nil {
		return 0, 0, fmt.Errorf("sweep dead-letter: %w", err)
	}

	reclaimTag, err := q.pool.Exec(ctx,
		"UPDATE work_item SET state = $1, lease_until = NULL WHERE state = $2 AND lease_until <= $3 AND deliveries < $4",
		string(WorkReady), string(WorkLeased), nowUTC, maxDeliveries)
	if err != nil {
		return 0, 0, fmt.Errorf("sweep reclaim: %w", err)
	}

	return int(reclaimTag.RowsAffected()), int(deadTag.RowsAffected()), nil
}

// PurgeDead deletes dead items created before cutoff and reports the count.
func (q *PostgresQueue) PurgeDead(ctx context.Context, cutoff time.Time) (int, error) {
	tag, err := q.pool.Exec(ctx,
		"DELETE FROM work_item WHERE state = $1 AND created_at < $2", string(WorkDead), cutoff.UTC())
	if err != nil {
		return 0, fmt.Errorf("purge dead work items: %w", err)
	}

	return int(tag.RowsAffected()), nil
}

// Stats returns the queue-depth snapshot: a count per state plus the age of the
// oldest waiting item.
func (q *PostgresQueue) Stats(ctx context.Context, now time.Time) (QueueStats, error) {
	rows, err := q.pool.Query(ctx, "SELECT state, COUNT(*) FROM work_item GROUP BY state")
	if err != nil {
		return QueueStats{}, fmt.Errorf("queue stats: %w", err)
	}

	stats, err := scanPGStateCounts(rows)
	if err != nil {
		return QueueStats{}, err
	}

	var oldest *time.Time
	if scanErr := q.pool.QueryRow(ctx,
		"SELECT MIN(created_at) FROM work_item WHERE state = $1", string(WorkReady)).Scan(&oldest); scanErr != nil {
		return QueueStats{}, fmt.Errorf("queue stats oldest: %w", scanErr)
	}

	if oldest != nil {
		stats.OldestReadyAge = now.Sub(*oldest)
	}

	return stats, nil
}

// setState runs a state-changing UPDATE and maps a zero-row result to
// ErrWorkItemNotFound, so a hold or resume against a missing or wrong-state row
// is legible rather than silent.
func (q *PostgresQueue) setState(ctx context.Context, operation, id, query string, args ...any) error {
	tag, err := q.pool.Exec(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s work item %s: %w", operation, id, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrWorkItemNotFound, id)
	}

	return nil
}

// scanPGWorkItems reads every row of a work-item query into a slice.
func scanPGWorkItems(rows pgx.Rows) ([]WorkItem, error) {
	defer rows.Close()

	var items []WorkItem

	for rows.Next() {
		item, err := scanPGWorkItem(rows)
		if err != nil {
			return nil, err
		}

		items = append(items, item)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan work items: %w", err)
	}

	return items, nil
}

// scanPGWorkItem reads one pgWorkItemColumns row back into a WorkItem.
func scanPGWorkItem(rows pgx.Rows) (WorkItem, error) {
	var (
		item       WorkItem
		state      string
		leaseUntil *time.Time
	)

	err := rows.Scan(&item.ID, &item.DedupKey, &item.Payload, &state,
		&item.AvailableAt, &leaseUntil, &item.Deliveries, &item.CreatedAt, &item.HoldKey)
	if err != nil {
		return WorkItem{}, fmt.Errorf("scan work item: %w", err)
	}

	item.State = WorkState(state)
	if leaseUntil != nil {
		item.LeaseUntil = *leaseUntil
	}

	return item, nil
}

// scanPGStateCounts folds a state/count result into a QueueStats, leaving
// OldestReadyAge for the caller to fill.
func scanPGStateCounts(rows pgx.Rows) (QueueStats, error) {
	defer rows.Close()

	var stats QueueStats

	for rows.Next() {
		var (
			state string
			count int
		)

		if err := rows.Scan(&state, &count); err != nil {
			return QueueStats{}, fmt.Errorf("scan state count: %w", err)
		}

		assignStateCount(&stats, WorkState(state), count)
	}

	if err := rows.Err(); err != nil {
		return QueueStats{}, fmt.Errorf("scan state counts: %w", err)
	}

	return stats, nil
}
