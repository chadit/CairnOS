package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// workItemSchema is the durable work-queue table (.agents/spec/14-open-decisions.md,
// S8) with IF NOT EXISTS so reopening is idempotent. The claim index covers the
// hot path: ready items ordered by when they became available. dedup_key is
// unique so an enqueue several nodes attempt lands exactly one row.
const workItemSchema = `
CREATE TABLE IF NOT EXISTS work_item (
    id           TEXT PRIMARY KEY,
    dedup_key    TEXT NOT NULL UNIQUE,
    payload      BLOB NOT NULL,
    state        TEXT NOT NULL,
    available_at TIMESTAMP NOT NULL,
    lease_until  TIMESTAMP,
    deliveries   INTEGER NOT NULL DEFAULT 0,
    created_at   TIMESTAMP NOT NULL,
    hold_key     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_work_item_claim ON work_item(state, available_at);
`

// workItemColumns is the SELECT list every work-item query shares, in
// scanWorkItem order.
const workItemColumns = "id, dedup_key, payload, state, available_at, lease_until, deliveries, created_at, hold_key"

// SQLiteQueue is the single-node WorkQueue backend. The SQLite write lock
// serializes claims, so it needs no SKIP LOCKED; the design pairs it with one
// worker (.agents/spec/14-open-decisions.md, S8).
type SQLiteQueue struct {
	db *sql.DB
}

// OpenSQLiteQueue opens (creating if needed) the work-queue database at path and
// applies the schema, with the same WAL and busy_timeout pragmas the schedule
// and run stores use so a reader never blocks the writer.
func OpenSQLiteQueue(path string) (*SQLiteQueue, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open work-queue database %q: %w", path, err)
	}

	// Schema creation is process-lifetime initialization with no request to
	// inherit a context from; OpenSQLiteQueue carries no ctx on purpose.
	setupCtx := context.Background()

	if _, execErr := db.ExecContext(setupCtx, workItemSchema); execErr != nil {
		return nil, fmt.Errorf("create work-queue schema: %w", errors.Join(execErr, db.Close()))
	}

	// The poll-cursor table rides the same file as the work-item table, so a
	// poll fire persists its cursor through the one queue handle it enqueues on.
	if _, execErr := db.ExecContext(setupCtx, pollCursorSchema); execErr != nil {
		return nil, fmt.Errorf("create poll-cursor schema: %w", errors.Join(execErr, db.Close()))
	}

	return &SQLiteQueue{db: db}, nil
}

// Close releases the underlying database.
func (q *SQLiteQueue) Close() error {
	if err := q.db.Close(); err != nil {
		return fmt.Errorf("close work-queue database: %w", err)
	}

	return nil
}

// Enqueue inserts a ready item, exactly-once on dedup_key. ON CONFLICT DO
// NOTHING makes a duplicate a no-op, so several nodes firing the same cron
// occurrence land one row; RowsAffected reports whether this call was the one.
func (q *SQLiteQueue) Enqueue(ctx context.Context, item *WorkItem) (bool, error) {
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

	result, err := q.db.ExecContext(ctx,
		`INSERT INTO work_item (id, dedup_key, payload, state, available_at, lease_until, deliveries, created_at)
		 VALUES (?, ?, ?, ?, ?, NULL, 0, ?)
		 ON CONFLICT(dedup_key) DO NOTHING`,
		id, item.DedupKey, item.Payload, string(WorkReady), storedTime(availableAt), storedTime(createdAt))
	if err != nil {
		return false, fmt.Errorf("enqueue work item %s: %w", item.DedupKey, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("enqueue work item %s: rows affected: %w", item.DedupKey, err)
	}

	return affected == 1, nil
}

// OccurrencePending reports whether the schedule has an unfinished occurrence.
// It matches the dedup-key prefix with substr rather than LIKE so a schedule id
// never needs escaping.
//
// A leased row counts only while its lease is live: a lease nobody renewed
// outlived the worker that took it, so counting it would let one killed process
// block the schedule.
func (q *SQLiteQueue) OccurrencePending(ctx context.Context, scheduleID string, now time.Time) (bool, error) {
	prefix := OccurrencePrefix(scheduleID)

	var pending bool

	err := q.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM work_item
		 WHERE substr(dedup_key, 1, length(?)) = ?
		   AND (state IN (?, ?) OR (state = ? AND lease_until > ?)))`,
		prefix, prefix,
		string(WorkReady), string(WorkHeld), string(WorkLeased), storedTime(now)).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("pending occurrence for schedule %s: %w", scheduleID, err)
	}

	return pending, nil
}

// Claim leases up to limit ready, due items. It selects candidates then updates
// them in one transaction; the SQLite write lock means no other claim runs
// concurrently, so the selected rows are this worker's. The state='ready' guard
// on the update is belt-and-suspenders for the documented single-worker case.
func (q *SQLiteQueue) Claim(ctx context.Context, now time.Time, ackWait time.Duration, limit int) ([]WorkItem, error) {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claim begin: %w", err)
	}
	defer rollbackUnlessDone(tx)

	rows, err := tx.QueryContext(ctx,
		"SELECT "+workItemColumns+" FROM work_item WHERE state = ? AND available_at <= ? ORDER BY available_at ASC, id ASC LIMIT ?",
		string(WorkReady), storedTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("claim select: %w", err)
	}

	items, err := scanWorkItems(rows)
	if err != nil {
		return nil, err
	}

	if len(items) == 0 {
		return nil, nil
	}

	leaseUntil := now.Add(ackWait)
	if err := leaseClaimed(ctx, tx, items, leaseUntil); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("claim commit: %w", err)
	}

	for idx := range items {
		items[idx].State = WorkLeased
		items[idx].LeaseUntil = leaseUntil
		items[idx].Deliveries++
	}

	return items, nil
}

// leaseClaimed marks the selected items leased in one UPDATE with an IN list, so
// a prefetch of N rows costs one statement rather than N.
func leaseClaimed(ctx context.Context, tx *sql.Tx, items []WorkItem, leaseUntil time.Time) error {
	args := slices.Grow([]any{string(WorkLeased), storedTime(leaseUntil), string(WorkReady)}, len(items))

	for idx := range items {
		args = append(args, items[idx].ID)
	}

	// #nosec G202 -- the id list is a comma-joined run of "?" generated from the
	// item count; every value, the states and ids alike, binds as a parameter.
	query := "UPDATE work_item SET state = ?, lease_until = ?, deliveries = deliveries + 1 WHERE state = ? AND id IN (" +
		idPlaceholders(len(items)) + ")"

	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("claim lease: %w", err)
	}

	return nil
}

// Renew pushes the lease deadline of the given leased items out, the heartbeat
// a worker sends while its runs are still going. An item that already settled
// no longer matches the leased guard, so a renewal racing a settle quietly does
// nothing.
func (q *SQLiteQueue) Renew(ctx context.Context, ids []string, leaseUntil time.Time) error {
	if len(ids) == 0 {
		return nil
	}

	args := slices.Grow([]any{storedTime(leaseUntil), string(WorkLeased)}, len(ids))

	for _, id := range ids {
		args = append(args, id)
	}

	// #nosec G202 -- the id list is a comma-joined run of "?" generated from the
	// id count; every value, the state and ids alike, binds as a parameter.
	query := "UPDATE work_item SET lease_until = ? WHERE state = ? AND id IN (" + idPlaceholders(len(ids)) + ")"

	if _, err := q.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("renew %d work item leases: %w", len(ids), err)
	}

	return nil
}

// idPlaceholders builds the comma-joined run of "?" an IN list of n ids needs.
func idPlaceholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

// Ack removes a finished item. A missing row is not an error: a lease sweep may
// have reclaimed it first.
func (q *SQLiteQueue) Ack(ctx context.Context, id string) error {
	if _, err := q.db.ExecContext(ctx, "DELETE FROM work_item WHERE id = ?", id); err != nil {
		return fmt.Errorf("ack work item %s: %w", id, err)
	}

	return nil
}

// Nak returns a failed item to ready with a backoff delay, or dead-letters it
// once Deliveries has reached maxDeliveries. The delivery count was already
// incremented at claim, so it reflects attempts made.
func (q *SQLiteQueue) Nak(ctx context.Context, id string, now time.Time, backoff time.Duration, maxDeliveries int) (WorkState, error) {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("nak begin: %w", err)
	}
	defer rollbackUnlessDone(tx)

	var deliveries int
	if scanErr := tx.QueryRowContext(ctx, "SELECT deliveries FROM work_item WHERE id = ?", id).Scan(&deliveries); scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: %s", ErrWorkItemNotFound, id)
		}

		return "", fmt.Errorf("nak read %s: %w", id, scanErr)
	}

	next := WorkReady
	if deliveries >= maxDeliveries {
		next = WorkDead
	}

	if _, execErr := tx.ExecContext(ctx,
		"UPDATE work_item SET state = ?, available_at = ?, lease_until = NULL WHERE id = ?",
		string(next), storedTime(now.Add(backoff)), id); execErr != nil {
		return "", fmt.Errorf("nak update %s: %w", id, execErr)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("nak commit: %w", err)
	}

	return next, nil
}

// Hold parks a leased item so the sweep does not redeliver it until Resume,
// recording holdKey (the re-auth provider) so a resume probe knows which
// credential the item is waiting on.
func (q *SQLiteQueue) Hold(ctx context.Context, id, holdKey string) error {
	return q.setState(ctx, "hold", id,
		"UPDATE work_item SET state = ?, lease_until = NULL, hold_key = ? WHERE id = ?",
		string(WorkHeld), holdKey, id)
}

// Resume returns a held item to ready and clears its hold key. It leaves
// available_at alone: a held item was leased, so its available_at already sits in
// the past, which makes the resumed item immediately claimable without coupling
// resume to a clock.
func (q *SQLiteQueue) Resume(ctx context.Context, id string) error {
	return q.setState(ctx, "resume", id,
		"UPDATE work_item SET state = ?, lease_until = NULL, hold_key = '' WHERE id = ? AND state = ?",
		string(WorkReady), id, string(WorkHeld))
}

// ListHeld returns every held item with its hold key, so a resume probe can
// check the credential each one is waiting on (D12).
func (q *SQLiteQueue) ListHeld(ctx context.Context) ([]WorkItem, error) {
	rows, err := q.db.QueryContext(ctx,
		"SELECT "+workItemColumns+" FROM work_item WHERE state = ? ORDER BY id ASC", string(WorkHeld))
	if err != nil {
		return nil, fmt.Errorf("list held work items: %w", err)
	}

	return scanWorkItems(rows)
}

// SweepExpired reclaims leases past their deadline: those under the delivery
// limit go back to ready, those at it dead-letter. Two statements, both scoped
// to expired leases, so a healthy in-flight item is never touched.
func (q *SQLiteQueue) SweepExpired(ctx context.Context, now time.Time, maxDeliveries int) (int, int, error) {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("sweep begin: %w", err)
	}
	defer rollbackUnlessDone(tx)

	nowStr := storedTime(now)

	dead, err := execCount(ctx, tx,
		"UPDATE work_item SET state = ?, lease_until = NULL WHERE state = ? AND lease_until <= ? AND deliveries >= ?",
		string(WorkDead), string(WorkLeased), nowStr, maxDeliveries)
	if err != nil {
		return 0, 0, fmt.Errorf("sweep dead-letter: %w", err)
	}

	reclaimed, err := execCount(ctx, tx,
		"UPDATE work_item SET state = ?, lease_until = NULL WHERE state = ? AND lease_until <= ? AND deliveries < ?",
		string(WorkReady), string(WorkLeased), nowStr, maxDeliveries)
	if err != nil {
		return 0, 0, fmt.Errorf("sweep reclaim: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("sweep commit: %w", err)
	}

	return reclaimed, dead, nil
}

// PurgeDead deletes dead items created before cutoff and reports the count.
func (q *SQLiteQueue) PurgeDead(ctx context.Context, cutoff time.Time) (int, error) {
	result, err := q.db.ExecContext(ctx,
		"DELETE FROM work_item WHERE state = ? AND created_at < ?", string(WorkDead), storedTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("purge dead work items: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purge dead work items: rows affected: %w", err)
	}

	return int(affected), nil
}

// Stats returns the queue-depth snapshot: a count per state plus the age of the
// oldest waiting item.
func (q *SQLiteQueue) Stats(ctx context.Context, now time.Time) (QueueStats, error) {
	rows, err := q.db.QueryContext(ctx, "SELECT state, COUNT(*) FROM work_item GROUP BY state")
	if err != nil {
		return QueueStats{}, fmt.Errorf("queue stats: %w", err)
	}

	stats, err := scanStateCounts(rows)
	if err != nil {
		return QueueStats{}, err
	}

	// MIN(created_at) is an aggregate expression, so it loses the column's
	// TIMESTAMP affinity and comes back as the raw stored string; scan it as text
	// and parse it with the shared layout rather than relying on driver coercion.
	var oldest sql.NullString
	if scanErr := q.db.QueryRowContext(ctx,
		"SELECT MIN(created_at) FROM work_item WHERE state = ?", string(WorkReady)).Scan(&oldest); scanErr != nil {
		return QueueStats{}, fmt.Errorf("queue stats oldest: %w", scanErr)
	}

	if oldest.Valid {
		parsed, parseErr := time.Parse(storedTimeLayout, oldest.String)
		if parseErr != nil {
			return QueueStats{}, fmt.Errorf("queue stats parse oldest: %w", parseErr)
		}

		stats.OldestReadyAge = now.Sub(parsed)
	}

	return stats, nil
}

// setState runs a state-changing UPDATE and maps a zero-row result to
// ErrWorkItemNotFound, so a hold or resume against a missing or wrong-state row
// is legible rather than silent.
func (q *SQLiteQueue) setState(ctx context.Context, operation, id, query string, args ...any) error {
	result, err := q.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s work item %s: %w", operation, id, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s work item %s: rows affected: %w", operation, id, err)
	}

	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrWorkItemNotFound, id)
	}

	return nil
}

// rollbackUnlessDone abandons tx on an early return. Once Commit has run, the
// rollback reports sql.ErrTxDone, which carries no news. Any other failure is
// logged, because the deferred call has no caller left to hand it to and the
// function is already returning its own error.
func rollbackUnlessDone(tx *sql.Tx) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		slog.Warn("work queue rollback failed", "error", err)
	}
}

// execCount runs a write statement in tx and returns its affected-row count.
func execCount(ctx context.Context, tx *sql.Tx, query string, args ...any) (int, error) {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("exec: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}

	return int(affected), nil
}

// scanWorkItems reads every row of a work-item query into a slice.
func scanWorkItems(rows *sql.Rows) ([]WorkItem, error) {
	defer closeRows(rows)

	var items []WorkItem

	for rows.Next() {
		item, err := scanWorkItem(rows)
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

// scanWorkItem reads one workItemColumns row back into a WorkItem.
func scanWorkItem(rows *sql.Rows) (WorkItem, error) {
	var (
		item        WorkItem
		state       string
		availableAt sql.NullTime
		leaseUntil  sql.NullTime
		createdAt   sql.NullTime
	)

	err := rows.Scan(&item.ID, &item.DedupKey, &item.Payload, &state,
		&availableAt, &leaseUntil, &item.Deliveries, &createdAt, &item.HoldKey)
	if err != nil {
		return WorkItem{}, fmt.Errorf("scan work item: %w", err)
	}

	item.State = WorkState(state)
	item.AvailableAt = availableAt.Time
	item.LeaseUntil = leaseUntil.Time
	item.CreatedAt = createdAt.Time

	return item, nil
}

// scanStateCounts folds a state/count result into a QueueStats, leaving
// OldestReadyAge for the caller to fill.
func scanStateCounts(rows *sql.Rows) (QueueStats, error) {
	defer closeRows(rows)

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

// assignStateCount routes one state's count into the matching QueueStats field.
func assignStateCount(stats *QueueStats, state WorkState, count int) {
	switch state {
	case WorkReady:
		stats.Ready = count
	case WorkLeased:
		stats.Leased = count
	case WorkHeld:
		stats.Held = count
	case WorkDead:
		stats.Dead = count
	}
}
