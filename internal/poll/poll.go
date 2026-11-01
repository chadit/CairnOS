// Package poll turns one scheduled cron fire into workflow events. A
// [workflow.poll] table names a Source; on each fire the poll runner asks the
// Source what changed since the last observation and enqueues one run per new
// event. This package holds the Source seam and the source registry, so the
// next source is a small adapter; the fire path that wraps the scheduler runner
// lives in internal/cli.
package poll

import (
	"context"
)

// Event is one observed app event: a stable id for dedup plus the input values
// it offers the workflow.
type Event struct {
	// Inputs are the values the event offers. The fire path filters them to the
	// workflow's declared inputs before enqueue; the engine has final say at run
	// time.
	Inputs map[string]any

	// ID is stable per source, so the queue dedups repeat observations of the
	// same event down to one run.
	ID string
}

// Source polls an app for events newer than cursor and returns them with the
// advanced cursor. A cursor of "" means no prior poll.
type Source interface {
	Poll(ctx context.Context, params map[string]any, cursor string) (events []Event, next string, err error)
}

// Sources is the registry of poll sources keyed by the [workflow.poll] source
// name. The wiring builds it once from the runner registry and only reads it
// after, so a plain map needs no lock. A fire-time lookup that misses reports
// ErrUnknownSource with the name, which is a config value, not a secret.
type Sources map[string]Source

// BuiltinSourceInputs returns the input keys every event from the named source
// carries, and false when no wiring builds a source under that name. A poll
// fire supplies only those keys, so `cairnos schedule add` uses this to warn on
// an unknown source and to refuse a required input the source never emits.
func BuiltinSourceInputs(name string) ([]string, bool) {
	if name != GitHubSourceName {
		return nil, false
	}

	return []string{keyNumber, keyTitle, keyBody, keyURL, keyAuthor, keyCreatedAt}, true
}
