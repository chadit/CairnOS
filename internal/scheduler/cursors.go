package scheduler

import "context"

// PollCursors persists one opaque cursor string per schedule so re-polls do not
// re-read events whose acked queue rows are already gone. The poll fire path
// reads the cursor before polling a source and writes the advanced cursor after
// enqueuing the events.
type PollCursors interface {
	// GetCursor returns the schedule's stored cursor, or ErrCursorNotFound when
	// the schedule has never polled. The empty-cursor first poll is the caller's
	// concern, not a stored value.
	GetCursor(ctx context.Context, scheduleID string) (string, error)

	// SetCursor upserts the schedule's cursor, so the next poll reads events
	// newer than this write.
	SetCursor(ctx context.Context, scheduleID, cursor string) error
}

// Both queue backends carry the poll cursors table beside the work-item table,
// so a poll fire persists its cursor through the same handle it enqueues on. The
// assertion surfaces a signature drift here rather than at the wiring call site.
var (
	_ PollCursors = (*SQLiteQueue)(nil)
	_ PollCursors = (*PostgresQueue)(nil)
)
