package scheduler

import "errors"

// Sentinel errors for the scheduler package.
var (
	// ErrScheduleNotFound reports a Remove against an id or workflow name
	// that matches no persisted schedule.
	ErrScheduleNotFound = errors.New("schedule not found")

	// ErrBadCron reports a schedule expression that is neither a supported
	// @-shorthand nor a parseable 5-field or 6-field cron expression.
	ErrBadCron = errors.New("bad cron expression")

	// ErrTooFrequent reports a well-formed expression whose fires land closer
	// together than the scheduler's floor, the mistake the optional seconds
	// field makes easy to write.
	ErrTooFrequent = errors.New("schedule fires too often")

	// ErrNoRunner reports a Scheduler built without a Runner; a daemon that
	// cannot fire anything is a wiring bug, caught before the loop starts.
	ErrNoRunner = errors.New("no runner configured")

	// ErrNoStore reports a Scheduler built without a Schedules store.
	ErrNoStore = errors.New("no schedule store configured")

	// ErrWorkItemNotFound reports a queue operation (nak, hold, resume) against
	// an id that matches no work item, or one in the wrong state for the change.
	ErrWorkItemNotFound = errors.New("work item not found")

	// ErrCursorNotFound reports a GetCursor for a schedule that has never
	// polled, so the poll fire path reads it as the empty first-poll cursor.
	ErrCursorNotFound = errors.New("poll cursor not found")
)
