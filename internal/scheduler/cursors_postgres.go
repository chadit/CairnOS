package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// pgPollCursorSchema is the Postgres poll-cursor table: one opaque cursor per
// schedule with a timestamptz stamp, applied on the shared pool at open with
// IF NOT EXISTS so several nodes opening it stay idempotent.
const pgPollCursorSchema = `
CREATE TABLE IF NOT EXISTS poll_cursors (
    schedule_id TEXT PRIMARY KEY,
    cursor      TEXT NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL
);
`

// GetCursor returns the schedule's stored poll cursor, mapping a missing row to
// ErrCursorNotFound so the fire path reads it as the empty first-poll cursor.
func (q *PostgresQueue) GetCursor(ctx context.Context, scheduleID string) (string, error) {
	var cursor string

	err := q.pool.QueryRow(ctx,
		"SELECT cursor FROM poll_cursors WHERE schedule_id = $1", scheduleID).Scan(&cursor)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("%w: %s", ErrCursorNotFound, scheduleID)
		}

		return "", fmt.Errorf("get poll cursor %s: %w", scheduleID, err)
	}

	return cursor, nil
}

// SetCursor upserts the schedule's poll cursor. ON CONFLICT keeps one row per
// schedule, so an advanced cursor overwrites the prior one in place.
func (q *PostgresQueue) SetCursor(ctx context.Context, scheduleID, cursor string) error {
	_, err := q.pool.Exec(ctx,
		`INSERT INTO poll_cursors (schedule_id, cursor, updated_at)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (schedule_id) DO UPDATE SET cursor = EXCLUDED.cursor, updated_at = EXCLUDED.updated_at`,
		scheduleID, cursor, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("set poll cursor %s: %w", scheduleID, err)
	}

	return nil
}
