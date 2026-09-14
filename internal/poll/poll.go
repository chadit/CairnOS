// Package poll turns one scheduled cron fire into workflow events. A
// [workflow.poll] table names a Source; on each fire the poll runner asks the
// Source what changed since the last observation and enqueues one run per new
// event. This package holds the Source seam and the source registry, so the
// next source is a small adapter; the fire path that wraps the scheduler runner
// lives in internal/cli (.agents/spec/parity/triggers-webhooks.md, "Poll
// triggers").
package poll

import (
	"context"
	"fmt"
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
// name. The wiring builds it from the runner registry; Get is the fire-time
// lookup a poll workflow's source name resolves through.
type Sources map[string]Source

// Get returns the source registered under name, or ErrUnknownSource when a
// [workflow.poll] table names one that was never registered. The name is a
// config value, not a secret, so it rides the error for a clear message.
func (s Sources) Get(name string) (Source, error) {
	src, ok := s[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownSource, name)
	}

	return src, nil
}

// BuiltinSourceNames lists the poll sources the wiring builds from the runner
// registry, so `cairnos schedule add` can warn at add time when a
// [workflow.poll] names a source no wiring will register. It is the add-time
// counterpart to the registry-built source map the fire path resolves through.
func BuiltinSourceNames() []string {
	return []string{GitHubSourceName}
}
