package scheduler

import "errors"

// Sentinel errors for the scheduler package.
var (
	// ErrScheduleNotFound reports a Remove against an id or workflow name
	// that matches no persisted schedule.
	ErrScheduleNotFound = errors.New("schedule not found")

	// ErrBadCron reports a schedule expression that is neither a supported
	// @-shorthand nor a parseable 5-field cron expression.
	ErrBadCron = errors.New("bad cron expression")

	// ErrNoRunner reports a Scheduler built without a Runner; a daemon that
	// cannot fire anything is a wiring bug, caught before the loop starts.
	ErrNoRunner = errors.New("no runner configured")

	// ErrNoStore reports a Scheduler built without a Schedules store.
	ErrNoStore = errors.New("no schedule store configured")
)
