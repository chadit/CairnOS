package poll

import (
	"context"
	"fmt"
)

// Cursors reads and writes one opaque cursor per schedule. Get returns "" when
// the schedule has never polled, so the first poll runs with an empty cursor and
// the source decides whether to backfill.
type Cursors interface {
	Get(ctx context.Context, scheduleID string) (string, error)
	Set(ctx context.Context, scheduleID, cursor string) error
}

// EnqueueEvent persists one work item for a polled event. The caller builds it
// over the work queue's dedup enqueue, so an overlapping poll window that
// re-observes an event lands no second row.
type EnqueueEvent func(ctx context.Context, event Event) error

// DispatchInput carries one poll cycle's dependencies. Paused is read once by the
// caller so the pause store stays a caller concern; Dispatch only honors it.
type DispatchInput struct {
	Source     Source
	Cursors    Cursors
	Params     map[string]any
	Enqueue    EnqueueEvent
	ScheduleID string
	Paused     bool
}

// Dispatch runs one poll cycle. A paused schedule polls nothing and holds its
// cursor, so events wait for resume. A live schedule reads the cursor, polls,
// enqueues one item per event, then advances it. A source or enqueue error
// returns without advancing, so the next poll re-reads the same window and
// dedup keeps queued events from double-firing.
func Dispatch(ctx context.Context, input DispatchInput) error {
	if input.Paused {
		return nil
	}

	cursor, err := input.Cursors.Get(ctx, input.ScheduleID)
	if err != nil {
		return fmt.Errorf("read poll cursor: %w", err)
	}

	events, next, err := input.Source.Poll(ctx, input.Params, cursor)
	if err != nil {
		return fmt.Errorf("poll source: %w", err)
	}

	for idx := range events {
		if enqErr := input.Enqueue(ctx, events[idx]); enqErr != nil {
			return fmt.Errorf("enqueue poll event %s: %w", events[idx].ID, enqErr)
		}
	}

	if err := input.Cursors.Set(ctx, input.ScheduleID, next); err != nil {
		return fmt.Errorf("advance poll cursor: %w", err)
	}

	return nil
}
