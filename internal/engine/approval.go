package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// CodeApprovalDenied marks an approval action an operator denied. It engages
// on_error and error_workflow like any action failure
// (.agents/spec/parity/workflow-language.md, "Human approval").
const CodeApprovalDenied = "action.approval_denied"

// detailComment carries a denier's note on the action error, so history shows
// why an approval was refused.
const detailComment = "comment"

// approvalAttempt runs a human-approval action against its decision record. No
// row yet means the gate is fresh: it persists a pending row and parks the run
// as held. A decided row resolves the action: approval succeeds with the
// decider's identity, denial fails with action.approval_denied. A still-pending
// row (a resume before deciding) re-parks without writing a second gate
// (.agents/spec/parity/workflow-language.md, "Human approval").
func (e *Engine) approvalAttempt(ctx context.Context, runID string, action *workflow.Action, inputs, outputs map[string]any) attemptOutcome {
	// The read detaches from cancellation so a run being torn down still sees the
	// decision it already recorded, the same reason persistAction writes detached.
	decision, err := e.store.GetApproval(context.WithoutCancel(ctx), runID, action.Name)
	if errors.Is(err, store.ErrApprovalNotFound) {
		return e.parkApproval(ctx, runID, action, inputs, outputs)
	}

	if err != nil {
		return attemptOutcome{attempts: 1, cerr: asPrecondition(fmt.Errorf("read approval gate: %w", err))}
	}

	switch decision.Status {
	case store.ApprovalApproved:
		return attemptOutcome{attempts: 1, result: runner.Result{Output: map[string]any{
			"approved": true,
			"actor":    decision.Actor,
			"comment":  decision.Comment,
		}}}
	case store.ApprovalDenied:
		return attemptOutcome{attempts: 1, cerr: deniedError(action, decision)}
	case store.ApprovalPending:
		return attemptOutcome{attempts: 1, waiting: true}
	default:
		return attemptOutcome{attempts: 1, waiting: true}
	}
}

// parkApproval records a fresh pending gate and reports waiting so the run
// parks. The message is resolved here (it can reference upstream outputs, which
// are already persisted) but validation guarantees it carries no secret, so the
// persisted gate never leaks a credential.
func (e *Engine) parkApproval(ctx context.Context, runID string, action *workflow.Action, inputs, outputs map[string]any) attemptOutcome {
	message, err := e.resolveApprovalMessage(ctx, action, inputs, outputs)
	if err != nil {
		return attemptOutcome{attempts: 1, cerr: asPrecondition(err)}
	}

	gate := &store.Approval{
		RunID:       runID,
		ActionName:  action.Name,
		WorkspaceID: store.WorkspaceFromContext(ctx),
		Status:      store.ApprovalPending,
		Message:     message,
	}

	if err := e.store.CreateApproval(context.WithoutCancel(ctx), gate); err != nil {
		return attemptOutcome{attempts: 1, cerr: asPrecondition(fmt.Errorf("record approval gate: %w", err))}
	}

	return attemptOutcome{attempts: 1, waiting: true}
}

// resolveApprovalMessage interpolates an approval action's message at launch
// under the same reference rules as an agent goal: a whole reference keeps its
// value rendered to text, embedded references splice scalars. An empty message
// resolves to the empty string.
func (e *Engine) resolveApprovalMessage(ctx context.Context, action *workflow.Action, inputs, outputs map[string]any) (string, error) {
	if action.Message == "" {
		return "", nil
	}

	value, err := resolveString(action.Message, e.resolver(ctx, inputs, outputs))
	if err != nil {
		return "", fmt.Errorf("approval message: %w", err)
	}

	if text, isText := value.(string); isText {
		return text, nil
	}

	rendered, err := renderScalar(value)
	if err != nil {
		return "", errf(CodePreconditionFailed, "approval action %q message: %v", action.Name, err)
	}

	return rendered, nil
}

// parkAction persists the ActionWaiting row for a parked approval and reports
// the waiting event, so the scheduling loop quiesces the run to held. It closes
// the instrumentation span the running transition opened, since the action
// stops executing until the run resumes.
func (e *Engine) parkAction(ctx context.Context, runID string, action *workflow.Action, started time.Time, finishAction func(*store.ActionResult)) actionEvent {
	waiting := &store.ActionResult{
		RunID:      runID,
		ActionName: action.Name,
		Runner:     runnerLabel(action),
		Status:     store.ActionWaiting,
		StartedAt:  started,
	}

	e.persistAction(ctx, waiting)
	finishAction(waiting)

	return actionEvent{name: action.Name, status: store.ActionWaiting}
}

// deniedError builds the action.approval_denied failure from a denial decision,
// naming the decider and carrying any note in Details.
func deniedError(action *workflow.Action, decision *store.Approval) *cairnerr.Error {
	actor := decision.Actor
	if actor == "" {
		actor = "an operator"
	}

	cerr := errf(CodeApprovalDenied, "approval action %q was denied by %s", action.Name, actor)

	if decision.Comment != "" {
		return cerr.WithDetail(detailComment, decision.Comment)
	}

	return cerr
}

// Seeding does not re-write the succeeded rows, so they stay byte-identical. A
// nil error means the resume drove the existing run. A run that is not held (a
// terminal run whose gate a failure or cancel orphaned by beating the park) is
// rejected with ErrRunNotHeld so it stays terminal
// (.agents/spec/parity/workflow-language.md, "Human approval").

// ResumeRun drives a held run forward after its approval was decided. It seeds
// the run's already-succeeded actions in memory, then re-runs the parked
// approval and anything downstream through the normal loop, where the approval
// reads its decision and succeeds or fails. The returned run is terminal, or
// held again when a second branch still awaits its own approval.
func (e *Engine) ResumeRun(ctx context.Context, held *store.Run, results []*store.ActionResult, flow *workflow.Workflow) (*store.Run, error) {
	if flow == nil {
		return nil, ErrNilWorkflow
	}

	if held == nil {
		return nil, ErrNilRun
	}

	// A terminal run must stay terminal. Seeding the DAG flips the run back to
	// running and re-fires every non-succeeded action, so refuse anything that is
	// not parked rather than resurrect a run whose gate outlived it.
	if held.Status != store.RunHeld {
		return nil, fmt.Errorf("%w: run %s is %s", ErrRunNotHeld, held.ID, held.Status)
	}

	// persistSeeds false is in-memory seeding: the pre-park succeeded rows keep
	// their bytes, while waiting and downstream actions re-run from pending.
	e.executeSeededDAG(ctx, flow, held, held.Inputs, succeededSeeds(flow, results), false)

	return held, nil
}
