package chat

import (
	"context"
	"fmt"

	"github.com/chadit/CairnOS/internal/store"
)

// ConfirmAction resolves a pending workflow proposal the chat agent raised
// (.agents/spec/14-open-decisions.md, D9). approve true fires the proposed
// workflow through the engine under normal governance and streams its run
// events; approve false denies it and nothing runs. Either way the gate
// clears, so a second ConfirmAction on the same session reports no pending
// action. out is closed exactly once on return; the caller ranges it until
// close and reads the returned error for the outcome.
//
// The scope check is the caller's job and happens before this method: a
// workflow trigger requires write:workflow on the token (the API enforces it at
// confirm time). By the time ConfirmAction runs, the token is already proven to
// carry the scope, so this method does the firing, not the authorizing.
func (a *Agent) ConfirmAction(ctx context.Context, sessionID string, approve bool, out chan<- Event) error {
	defer close(out)

	// Load the workspace-scoped session before touching the sessionID-keyed
	// pending map: a session in another workspace reads as not-found here, so a
	// tenant cannot confirm another tenant's staged proposal by guessing its
	// session id (.agents/spec/08-auth-and-access.md, "Chat confirm workspace
	// scoping"). pendingFor alone is keyed by sessionID only and would fire the
	// foreign proposal.
	if _, err := a.GetSession(ctx, sessionID); err != nil {
		return err
	}

	pending, waiting := a.pendingFor(sessionID)
	if !waiting {
		return errf(CodeNoPendingAction,
			"chat session %s has no action awaiting confirmation", sessionID)
	}

	a.clearPending(sessionID)

	if !approve {
		return a.recordDenial(ctx, sessionID, out)
	}

	return a.fireProposal(ctx, sessionID, &pending.proposal, out)
}

// recordDenial persists the denial as an assistant message and closes the turn,
// so the conversation history shows the user said no and nothing ran.
func (a *Agent) recordDenial(ctx context.Context, sessionID string, out chan<- Event) error {
	const denied = "Workflow trigger denied; nothing was run."

	if err := a.cfg.Store.AppendMessage(ctx, &store.ChatMessage{
		SessionID: sessionID,
		Role:      store.ChatRoleAssistant,
		Content:   denied,
	}); err != nil {
		return fmt.Errorf("persist denial: %w", err)
	}

	return emit(ctx, out, &Event{Type: EventTurnDone, Text: denied})
}

// fireProposal runs the confirmed workflow through the trigger, streaming
// run-started and run-finished events and closing with turn-done. The run goes
// through normal engine governance (Phase 5b budgets and pause) and at depth
// exactly one: the spawned run's own actions still obey the one-level
// no-recursion rule, so nothing it does starts a third loop
// (.agents/spec/15-agent-loop.md; .agents/spec/14-open-decisions.md, D9).
func (a *Agent) fireProposal(ctx context.Context, sessionID string, proposal *Proposal, out chan<- Event) error {
	if a.cfg.Trigger == nil {
		return errf(CodeTriggerUnavailable,
			"this node cannot trigger workflows from chat; no workflow trigger is configured")
	}

	runID, done, err := a.cfg.Trigger.Run(ctx, proposal.WorkflowName, proposal.Inputs)
	if err != nil {
		return fmt.Errorf("trigger workflow %q: %w", proposal.WorkflowName, err)
	}

	a.recordRunStart(ctx, sessionID, proposal.WorkflowName, runID)

	if err := emit(ctx, out, &Event{Type: EventRunStarted, RunID: runID}); err != nil {
		return err
	}

	return a.awaitRun(ctx, sessionID, runID, done, out)
}

// awaitRun waits for the fired run's terminal status, streams run-finished and
// turn-done, and records the outcome as an assistant message so the next turn
// sees what happened.
func (a *Agent) awaitRun(ctx context.Context, sessionID, runID string, done <-chan RunResult, out chan<- Event) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("chat confirm canceled: %w", ctx.Err())
	case result := <-done:
		status := result.Status
		if result.Err != nil {
			status = string(store.RunFailed)
		}

		summary := fmt.Sprintf("Workflow run %s finished as %s.", runID, status)
		a.recordAssistant(ctx, sessionID, summary)

		if err := emit(ctx, out, &Event{Type: EventRunFinished, RunID: runID, RunStatus: status}); err != nil {
			return err
		}

		return emit(ctx, out, &Event{Type: EventTurnDone, Text: summary})
	}
}

// recordRunStart notes the started run in the conversation, best-effort: a
// persistence failure here logs through the store's own error path rather than
// aborting a run that already began.
func (a *Agent) recordRunStart(ctx context.Context, sessionID, workflowName, runID string) {
	a.recordAssistant(ctx, sessionID,
		fmt.Sprintf("Confirmed. Started workflow %q as run %s.", workflowName, runID))
}

// recordAssistant appends one assistant message, swallowing the persistence
// error: the run has already started or finished, so a failed history write
// must not change the streamed outcome. The error is returned by the store's
// own logging path, not surfaced here.
func (a *Agent) recordAssistant(ctx context.Context, sessionID, content string) {
	_ = a.cfg.Store.AppendMessage(ctx, &store.ChatMessage{
		SessionID: sessionID,
		Role:      store.ChatRoleAssistant,
		Content:   content,
	})
}
