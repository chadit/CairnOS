package poll_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/poll"
	"github.com/chadit/CairnOS/internal/scheduler"
)

// Dispatch fixtures, named per the repo's goconst-in-tests policy.
const (
	dispatchScheduleID = "poll-sched-1"
	dispatchEventID    = "42"
	dispatchNextCursor = "2026-07-01T00:05:00Z"
	dispatchSeedCursor = "2026-06-30T00:00:00Z"
	dispatchClaimLimit = 10
)

// fakeSource returns a fixed event set (or error) and records how it was polled,
// so a test can assert both the enqueue result and that a paused cycle never
// polled.
type fakeSource struct {
	events     []poll.Event
	next       string
	err        error
	polls      int
	lastCursor string
}

func (s *fakeSource) Poll(_ context.Context, _ map[string]any, cursor string) ([]poll.Event, string, error) {
	s.polls++
	s.lastCursor = cursor

	if s.err != nil {
		return nil, "", s.err
	}

	return s.events, s.next, nil
}

// queueCursors adapts a scheduler queue's PollCursors to poll.Cursors, mapping
// the never-polled sentinel to the empty first-poll cursor, the same shape the
// cli wiring uses.
type queueCursors struct {
	queue scheduler.PollCursors
}

func (c queueCursors) Get(ctx context.Context, scheduleID string) (string, error) {
	cursor, err := c.queue.GetCursor(ctx, scheduleID)
	if err != nil {
		if errors.Is(err, scheduler.ErrCursorNotFound) {
			return "", nil
		}

		return "", fmt.Errorf("get cursor: %w", err)
	}

	return cursor, nil
}

func (c queueCursors) Set(ctx context.Context, scheduleID, cursor string) error {
	if err := c.queue.SetCursor(ctx, scheduleID, cursor); err != nil {
		return fmt.Errorf("set cursor: %w", err)
	}

	return nil
}

// newDispatchQueue opens a real sqlite work queue in a temp dir.
func newDispatchQueue(t *testing.T) *scheduler.SQLiteQueue {
	t.Helper()

	queue, err := scheduler.OpenSQLiteQueue(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}

	t.Cleanup(func() { _ = queue.Close() })

	return queue
}

// dispatchInput wires the injected source and pause verdict to a real queue's
// cursor store and a dedup-keyed enqueue closure.
func dispatchInput(queue *scheduler.SQLiteQueue, src poll.Source, paused bool) poll.DispatchInput {
	return poll.DispatchInput{
		ScheduleID: dispatchScheduleID,
		Source:     src,
		Paused:     paused,
		Cursors:    queueCursors{queue: queue},
		Enqueue: func(ctx context.Context, event poll.Event) error {
			if _, err := queue.Enqueue(ctx, &scheduler.WorkItem{
				DedupKey: scheduler.PollDedupKeyFor(dispatchScheduleID, event.ID),
				Payload:  []byte(`{"event":"` + event.ID + `"}`),
			}); err != nil {
				return fmt.Errorf("enqueue: %w", err)
			}

			return nil
		},
	}
}

func readyCount(t *testing.T, queue *scheduler.SQLiteQueue) int {
	t.Helper()

	stats, err := queue.Stats(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	return stats.Ready
}

func TestDispatchEnqueuesOnePerEventAndAdvancesCursor(t *testing.T) {
	t.Parallel()

	queue := newDispatchQueue(t)
	src := &fakeSource{events: []poll.Event{{ID: dispatchEventID}}, next: dispatchNextCursor}

	if err := poll.Dispatch(t.Context(), dispatchInput(queue, src, false)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if src.lastCursor != "" {
		t.Errorf("first poll cursor = %q, want empty (no prior poll)", src.lastCursor)
	}

	if got := readyCount(t, queue); got != 1 {
		t.Fatalf("ready = %d, want 1 queued row for one event", got)
	}

	claimed, err := queue.Claim(t.Context(), time.Now(), time.Minute, dispatchClaimLimit)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if len(claimed) != 1 || claimed[0].DedupKey != scheduler.PollDedupKeyFor(dispatchScheduleID, dispatchEventID) {
		t.Fatalf("claimed %+v, want one item keyed by the poll dedup key", claimed)
	}

	cursor, err := queue.GetCursor(t.Context(), dispatchScheduleID)
	if err != nil {
		t.Fatalf("get cursor: %v", err)
	}

	if cursor != dispatchNextCursor {
		t.Errorf("cursor = %q, want it advanced to %q", cursor, dispatchNextCursor)
	}
}

func TestDispatchDedupsRepeatEvent(t *testing.T) {
	t.Parallel()

	queue := newDispatchQueue(t)
	src := &fakeSource{events: []poll.Event{{ID: dispatchEventID}}, next: dispatchNextCursor}

	// Two cycles observe the same event while its row still exists: overlapping
	// poll windows dedup to one row, so the second enqueues nothing.
	for range 2 {
		if err := poll.Dispatch(t.Context(), dispatchInput(queue, src, false)); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
	}

	if got := readyCount(t, queue); got != 1 {
		t.Fatalf("ready = %d, want 1 after re-observing the same event", got)
	}
}

func TestDispatchPausedHoldsCursor(t *testing.T) {
	t.Parallel()

	queue := newDispatchQueue(t)
	src := &fakeSource{events: []poll.Event{{ID: dispatchEventID}}, next: dispatchNextCursor}

	if err := poll.Dispatch(t.Context(), dispatchInput(queue, src, true)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if src.polls != 0 {
		t.Errorf("paused cycle polled %d times, want 0", src.polls)
	}

	if got := readyCount(t, queue); got != 0 {
		t.Errorf("ready = %d, want 0 for a paused poll", got)
	}

	if _, err := queue.GetCursor(t.Context(), dispatchScheduleID); !errors.Is(err, scheduler.ErrCursorNotFound) {
		t.Errorf("cursor err = %v, want ErrCursorNotFound (unmoved)", err)
	}
}

func TestDispatchSourceErrorHoldsCursor(t *testing.T) {
	t.Parallel()

	queue := newDispatchQueue(t)

	if err := queue.SetCursor(t.Context(), dispatchScheduleID, dispatchSeedCursor); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}

	src := &fakeSource{err: errors.New("github down")}

	if err := poll.Dispatch(t.Context(), dispatchInput(queue, src, false)); err == nil {
		t.Fatal("dispatch with a failing source returned no error")
	}

	cursor, err := queue.GetCursor(t.Context(), dispatchScheduleID)
	if err != nil {
		t.Fatalf("get cursor: %v", err)
	}

	if cursor != dispatchSeedCursor {
		t.Errorf("cursor = %q, want it left at %q after a source error", cursor, dispatchSeedCursor)
	}
}
