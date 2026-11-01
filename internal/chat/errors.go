package chat

import (
	"errors"
	"fmt"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Canonical chat.* error codes the agent owns. They classify onto gRPC status
// the same family way the engine and agent codes do.
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

	// CodeBadWorkspace marks a turn whose request workspace id fails
	// ^[A-Za-z0-9_-]+$ before a context path is built, so a traversal-shaped id
	// never reads another workspace's context file.
	CodeBadWorkspace = "chat.bad_workspace_id"
)

// Sentinel errors for the chat package.
var (
	// ErrUnknownPersonality reports a CreateSession naming a personality the
	// node does not serve, so a typo fails at creation.
	ErrUnknownPersonality = errors.New("unknown personality")

	// ErrSkillNotFound is what a SkillSource returns for an unknown name; the
	// CLI adapter maps skills.ErrNotFound onto it.
	ErrSkillNotFound = errors.New("skill not found")
)

// errf builds a canonical chat error. A local constructor mirrors the agent
// loop's: these codes are the chat surface's own, returned unwrapped so
// wrapcheck stays satisfied where the handlers map them onto the wire.
func errf(code, format string, args ...any) *cairnerr.Error {
	return &cairnerr.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// refusal builds the permanent runner error a tool returns when a call cannot
// proceed as made, the one class every such refusal here carries. It returns
// the concrete type on purpose: a constructor never returns nil, so the
// typed-nil interface trap cannot apply, and a caller holding the result keeps
// Code and Class without a type assertion.
func refusal(code, message string) *runner.Error {
	return runner.NewError(runner.ClassPermanent, code, message)
}
