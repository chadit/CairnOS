package api

import (
	"context"
	"fmt"

	"github.com/chadit/CairnOS/internal/chat"
)

// triggerChat marks chat-confirmed runs in run history, so a run started from a
// conversation is distinguishable from a CLI, schedule, API, or A2A trigger.
const triggerChat = "chat"

// Routing through the manager keeps the run cancelable through CancelRun and
// correlates its run id.
//
// Depth-one holds structurally: the manager starts an ordinary workflow run,
// whose own actions obey the existing one-level no-recursion rule
// (.agents/spec/15-agent-loop.md), so nothing the spawned run does can start a
// third loop. The chat exception is the one place a loop triggers a workflow,
// and it is gated by the confirmation step before this adapter is ever called.

// ChatWorkflowTrigger adapts the run manager and the workflow resolver to
// chat.WorkflowTrigger, so a confirmed chat proposal fires a real run through
// the same engine, breakers, and governance every other run uses
// (.agents/spec/14-open-decisions.md, D9: the spawned run runs under Phase 5b
// budgets and pause, at depth exactly one).
type ChatWorkflowTrigger struct {
	manager  *Manager
	resolver *WorkflowResolver
}

// NewChatWorkflowTrigger wires the trigger over the run manager and a workflow
// resolver bound to the node's workflows directory.
func NewChatWorkflowTrigger(manager *Manager, resolver *WorkflowResolver) *ChatWorkflowTrigger {
	return &ChatWorkflowTrigger{manager: manager, resolver: resolver}
}

// Run resolves the named workflow, starts it under engine governance, and
// returns the run id with a channel delivering its terminal status once. A
// resolve failure or a governance refusal (an unknown workflow, a paused
// workflow, a budget breach) returns an error before any run id, which the chat
// agent surfaces as a failed confirmation.
func (t *ChatWorkflowTrigger) Run(ctx context.Context, name string, inputs map[string]any) (string, <-chan chat.RunResult, error) {
	flow, err := t.resolver.Resolve(name)
	if err != nil {
		return "", nil, fmt.Errorf("resolve workflow %q: %w", name, err)
	}

	handle, err := t.manager.StartRun(ctx, flow, inputs, false, triggerChat)
	if err != nil {
		return "", nil, fmt.Errorf("start workflow %q: %w", name, err)
	}

	done := make(chan chat.RunResult, 1)

	go forwardRunOutcome(handle, done)

	return handle.ID, done, nil
}

// forwardRunOutcome waits for the started run's terminal outcome and forwards
// it onto the chat trigger's done channel once. A run error carries through so
// the chat agent reports the run failed rather than succeeded.
func forwardRunOutcome(handle *RunHandle, done chan<- chat.RunResult) {
	outcome := <-handle.Outcome

	result := chat.RunResult{Err: outcome.Err}
	if outcome.Run != nil {
		result.Status = string(outcome.Run.Status)
	}

	done <- result
}
