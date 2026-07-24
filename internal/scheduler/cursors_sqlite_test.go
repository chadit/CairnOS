package scheduler_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/chadit/CairnOS/internal/scheduler"
)

// TestSQLiteCursorSurvivesReopen proves the poll cursor is durable: a cursor
// written before the queue closes reads back after reopening the same file, so a
// daemon restart re-polls from where it left off instead of backfilling.
func TestSQLiteCursorSurvivesReopen(t *testing.T) {
	t.Parallel()

	const (
		scheduleID = "sched-reopen"
		cursor     = "2026-06-13T12:00:00Z"
	)

	path := filepath.Join(t.TempDir(), "queue.db")

	first, err := scheduler.OpenSQLiteQueue(path)
	if err != nil {
		t.Fatalf("open first queue: %v", err)
	}

	if err := first.SetCursor(t.Context(), scheduleID, cursor); err != nil {
		t.Fatalf("set cursor: %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first queue: %v", err)
	}

	second, err := scheduler.OpenSQLiteQueue(path)
	if err != nil {
		t.Fatalf("reopen queue: %v", err)
	}

	t.Cleanup(func() { _ = second.Close() })

	got, err := second.GetCursor(t.Context(), scheduleID)
	if err != nil {
		t.Fatalf("get cursor after reopen: %v", err)
	}

	if got != cursor {
		t.Errorf("cursor after reopen = %q, want %q", got, cursor)
	}
}

// TestSQLiteCursorMissingIsNotFound checks an unread schedule reports
// ErrCursorNotFound, the sentinel the fire path reads as the empty first poll.
func TestSQLiteCursorMissingIsNotFound(t *testing.T) {
	t.Parallel()

	queue, err := scheduler.OpenSQLiteQueue(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}

	t.Cleanup(func() { _ = queue.Close() })

	if _, err := queue.GetCursor(t.Context(), "never-polled"); !errors.Is(err, scheduler.ErrCursorNotFound) {
		t.Fatalf("get missing cursor err = %v, want ErrCursorNotFound", err)
	}
}
