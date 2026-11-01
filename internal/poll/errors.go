package poll

import "errors"

// Sentinel errors for the poll package. Callers match with errors.Is; wrappers
// add the offending name or detail with %w.
var (
	// ErrUnknownSource reports a [workflow.poll] table naming a source that is
	// not registered. It surfaces at fire time, not at load, because the source
	// registry is runtime state, not file state.
	ErrUnknownSource = errors.New("unknown poll source")

	// ErrBadResult reports a source's backing runner returning a result whose
	// shape the adapter cannot read. Only a contract break between an op and its
	// adapter can produce it, so it fails the poll rather than silently dropping
	// events.
	ErrBadResult = errors.New("poll source returned an unreadable result")
)
