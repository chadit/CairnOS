package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// pollCursorSchema is the poll-cursor table: one opaque cursor per schedule,
// keyed by schedule id so a re-poll reads events newer than the last write. It
// shares the work-queue database file with the work_item table, applied at open
// with IF NOT EXISTS so reopening is idempotent.
const pollCursorSchema = `
CREATE TABLE IF NOT EXISTS poll_cursors (
    schedule_id TEXT PRIMARY KEY,
    cursor      TEXT NOT NULL,
    updated_at  TIMESTAMP NOT NULL
);
`

// GetCursor returns the schedule's stored poll cursor. A schedule that has never
// polled has no row, which surfaces as ErrCursorNotFound so the fire path reads
// it as the empty first-poll cursor.
func (q *SQLiteQueue) GetCursor(ctx context.Context, scheduleID string) (string, error) {
	var cursor string

	err := q.db.QueryRowContext(ctx,
		"SELECT cursor FROM poll_cursors WHERE schedule_id = ?", scheduleID).Scan(&cursor)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: %s", ErrCursorNotFound, scheduleID)
		}

		return "", fmt.Errorf("get poll cursor %s: %w", scheduleID, err)
	}

	return cursor, nil
}

// SetCursor upserts the schedule's poll cursor. The ON CONFLICT keeps one row
// per schedule, so an advanced cursor overwrites the prior one in place.
func (q *SQLiteQueue) SetCursor(ctx context.Context, scheduleID, cursor string) error {
	_, err := q.db.ExecContext(ctx,
		`INSERT INTO poll_cursors (schedule_id, cursor, updated_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(schedule_id) DO UPDATE SET cursor = excluded.cursor, updated_at = excluded.updated_at`,
		scheduleID, cursor, storedTime(time.Now()))
	if err != nil {
		return fmt.Errorf("set poll cursor %s: %w", scheduleID, err)
	}

	return nil
}
