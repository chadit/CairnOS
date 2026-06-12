package engine

import "github.com/chadit/CairnOS/internal/cairnerr"

// EventType names what an Event describes.
type EventType string

// Event types, in the order a run produces them: one run_started, any number
// of action_transition events, one run_finished.
const (
	EventRunStarted       EventType = "run_started"
	EventActionTransition EventType = "action_transition"
	EventRunFinished      EventType = "run_finished"
)

// Event is one run or action state change, emitted through Config.OnEvent so
// the API can stream a run live. Action fields are empty on run-level events;
// Status carries the action status on transitions and the run status on
// run_finished.
type Event struct {
	RunID   string
	Type    EventType
	Action  string
	Status  string
	Attempt int
	Err     *cairnerr.Error
}

// emit hands one event to the configured hook. Nil-safe: a missing hook
// costs one comparison. The hook runs synchronously on engine goroutines, so
// implementations have to come back promptly and never block; the API layer
// satisfies that with buffered channels that drop on overflow rather than
// stall the scheduler.
func (e *Engine) emit(evt *Event) {
	if e.cfg.OnEvent == nil {
		return
	}

	e.cfg.OnEvent(*evt)
}
