package agentloop

import (
	"fmt"

	"github.com/chadit/CairnOS/internal/cairnerr"
)

// Canonical agent.* error codes the loop owns
// (.agents/spec/15-agent-loop.md, "Termination").
const (
	// CodeMaxIterations marks a loop that used every driver iteration
	// without reaching a final answer.
	CodeMaxIterations = "agent.max_iterations"
	// CodeBudgetExhausted marks a loop whose estimated spend reached the
	// action's budget_usd.
	CodeBudgetExhausted = "agent.budget_exhausted"
	// CodeBadToolCall marks two consecutive driver turns that were
	// unparseable or named a tool outside the allowlist.
	CodeBadToolCall = "agent.bad_tool_call"
	// CodeDriverIncapable marks a driver that is not registered or lacks
	// native tool calling (runner.ToolCaller).
	CodeDriverIncapable = "agent.driver_incapable"
)

// errf builds a canonical loop error. A local constructor mirrors the
// engine's: these codes are the loop's own surface, not a foreign failure
// that needs wrapping context, which keeps wrapcheck satisfied where they
// return unwrapped.
func errf(code, format string, args ...any) *cairnerr.Error {
	return &cairnerr.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
