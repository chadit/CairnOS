package chat

import (
	"errors"
	"fmt"

	"github.com/chadit/CairnOS/internal/cairnerr"
)

// Canonical chat.* error codes the agent owns (.agents/spec/13-roadmap.md,
// Phase 9). They classify onto gRPC status the same family way the engine and
// agent codes do.
const (
	// CodeBudgetExhausted marks a turn whose estimated spend reached the turn's
	// budget cap before it answered.
	CodeBudgetExhausted = "chat.budget_exhausted"

	// CodeMaxIterations marks a turn that used every driver iteration without an
	// answer or a proposal.
	CodeMaxIterations = "chat.max_iterations"

	// CodeNoPendingAction marks a ConfirmAction against a session with no
	// proposal awaiting confirmation.
	CodeNoPendingAction = "chat.no_pending_action"

	// CodeTriggerUnavailable marks an approved proposal that cannot fire because
	// no workflow trigger is wired.
	CodeTriggerUnavailable = "chat.trigger_unavailable"

	// CodeDriverFailed marks a driver turn that errored, ending the chat turn.
	CodeDriverFailed = "chat.driver_failed"
)

// Sentinel errors for the chat package.
var (
	// ErrUnknownPersonality reports a CreateSession naming a personality the
	// node does not serve, so a typo fails at creation.
	ErrUnknownPersonality = errors.New("unknown personality")
)

// errf builds a canonical chat error. A local constructor mirrors the agent
// loop's: these codes are the chat surface's own, returned unwrapped so
// wrapcheck stays satisfied where the handlers map them onto the wire.
func errf(code, format string, args ...any) *cairnerr.Error {
	return &cairnerr.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
