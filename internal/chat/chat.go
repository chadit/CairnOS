// Package chat is the Phase 9 conversational front end over the Phase 1c
// agent loop (.agents/spec/13-roadmap.md, Phase 9): a conversation is a
// long-lived agent action whose tools are memory, the vault, and a gated
// run_workflow trigger. ChatAgent builds one turn over the conversation
// history, drives a runner.ToolCaller the same way internal/agentloop drives
// an agent action, and streams assistant text, tool-call events, and the D9
// confirmation-required signal back to the caller.
//
// # The D9 confirmation gate
//
// Agent loops are one level: an agent action's tools may not start a workflow
// (.agents/spec/15-agent-loop.md, "No recursion in v1"). The chat agent is the
// single, carefully-gated exception (D9 in .agents/spec/14-open-decisions.md).
// The run_workflow tool the chat agent holds does not run a workflow when the
// driver calls it. Instead the turn stops with a ConfirmationRequired event
// carrying the proposed workflow, its inputs, and the estimated budget, and
// waits for an explicit ConfirmAction from the user. Only an approval, on a
// token additionally carrying write:workflow, fires the workflow, and it runs
// under the normal engine governance (Phase 5b budgets and pause) at depth
// exactly one: the spawned run's own actions still obey the one-level rule, so
// nothing it does can start a third loop.
package chat

import (
	"time"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Tool names the chat agent offers. The trio is the Phase 9 tool set: memory
// (search and load), the vault read tool, and the gated workflow trigger
// (.agents/spec/13-roadmap.md, Phase 9).
const (
	// ToolMemory is the memory built-in (search, load, add).
	ToolMemory = builtins.MemoryName
	// ToolVaultQuery is the read-only vault graph query.
	ToolVaultQuery = builtins.VaultQueryName
	// ToolRunWorkflow is the D9 gated trigger: calling it proposes a workflow,
	// it never runs one directly.
	ToolRunWorkflow = RunWorkflowToolName
)

// EventType classifies one streamed turn event (.agents/spec/13-roadmap.md,
// Phase 9: "emits assistant text chunks, tool-call events,
// confirmation-required events, run-started/finished ..., turn-done").
type EventType string

// Turn event kinds, in the order a typical turn emits them.
const (
	// EventAssistantChunk carries a piece of the assistant's reply text.
	EventAssistantChunk EventType = "assistant_chunk"
	// EventToolCall reports the agent calling one of its read tools (memory or
	// vault); the gated trigger never produces this, it produces
	// EventConfirmationRequired instead.
	EventToolCall EventType = "tool_call"
	// EventConfirmationRequired is the D9 gate: the agent proposed a workflow
	// and the turn paused, waiting for ConfirmAction.
	EventConfirmationRequired EventType = "confirmation_required"
	// EventRunStarted fires when a confirmed workflow trigger starts a run.
	EventRunStarted EventType = "run_started"
	// EventRunFinished fires when that run reaches a terminal state.
	EventRunFinished EventType = "run_finished"
	// EventTurnDone closes the turn, whether it answered, paused for
	// confirmation, or failed.
	EventTurnDone EventType = "turn_done"
)

// Event is one entry in a turn's stream. Only the fields a given Type uses are
// set; the rest stay zero.
type Event struct {
	Type EventType

	// Text carries assistant reply text on EventAssistantChunk, and the final
	// assistant message on EventTurnDone.
	Text string

	// Tool is the tool name on EventToolCall.
	Tool string

	// ToolInput is the tool's arguments on EventToolCall.
	ToolInput map[string]any

	// Proposal is the pending workflow trigger on EventConfirmationRequired.
	Proposal *Proposal

	// RunID is the started or finished run id on EventRunStarted and
	// EventRunFinished.
	RunID string

	// RunStatus is the terminal run status on EventRunFinished.
	RunStatus string

	// Usage carries the turn's accumulated driver-and-tool token spend, set on
	// EventTurnDone so a caller reports real counts. The gRPC surface ignores it
	// until someone maps it; the OpenAI-compatible adapter reads it for its
	// usage block (.agents/spec/parity/openai-compat.md).
	Usage runner.Usage
}

// Proposal is the D9 confirmation payload: what the chat agent wants to run,
// with what inputs, and the estimated spend, so the user can approve or deny
// from full information (.agents/spec/13-roadmap.md, Phase 9 guardrail).
type Proposal struct {
	// WorkflowName is the workflow the agent proposes to run.
	WorkflowName string

	// Inputs are the run inputs the agent proposes.
	Inputs map[string]any

	// EstimatedBudgetUSD is the soft budget ceiling the spawned run would carry,
	// shown so a costly trigger is visible before it fires. It is the workflow's
	// declared budget when one exists, the turn's own cap otherwise.
	EstimatedBudgetUSD float64
}

// pendingProposal is the server-side state behind a confirmation gate: the
// proposal plus the timestamp it was raised, so a turn that resumes on
// ConfirmAction has everything it needs to fire the run. Held per session.
type pendingProposal struct {
	proposal  Proposal
	sessionID string
	raisedAt  time.Time
}
