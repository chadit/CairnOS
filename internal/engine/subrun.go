package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// triggerWorkflow marks a run created by a workflow-call action, so run history
// tells a child run apart from a root run and its other triggers.
const triggerWorkflow = "workflow"

// detailChildRunID names the failed child run on a workflow-call action's
// error, so history points from the parent action at the run that failed under
// it.
const detailChildRunID = "child_run_id"

// fieldRunID is the run-id key in the maps the engine hands a workflow: a
// workflow-call action's output and an error workflow's failure context.
const fieldRunID = "run_id"

// The depth caps recursion, and the ancestry both prints the offending chain
// and, by growing without bound, catches an A->B->A cycle without separate cycle
// tracking. callInfo is deliberately engine-internal: context.WithoutCancel
// copies context values, so the error-workflow fire a later slice adds keeps it,
// and any future context surgery must not strip it.

// callInfo travels a package-private context key down a workflow-call tree. It
// carries the run that owns the executing actions (the parent for any child a
// workflow-call action spawns), that run's nesting depth, and the chain of
// workflow names from the root down to it.
type callInfo struct {
	parentRunID string
	ancestry    []string
	depth       int
}

// callInfoKey is the unexported context key type, so no other package can set
// or read the ancestry.
type callInfoKey struct{}

// callInfoFrom reads the current call info, returning the zero value (a root
// run) when the context carries none.
func callInfoFrom(ctx context.Context) callInfo {
	info, _ := ctx.Value(callInfoKey{}).(callInfo)

	return info
}

// withCallInfo returns a context carrying info for the actions that run under
// it.
func withCallInfo(ctx context.Context, info callInfo) context.Context {
	return context.WithValue(ctx, callInfoKey{}, info)
}

// workflowCallAttempt runs a workflow-call action: it resolves the child by
// name, refuses recursion past MaxCallDepth, interpolates the inputs table, and
// runs the child as a real run through RunWorkflow. Governance and input
// validation need no new code here, because the recursive RunWorkflow re-applies
// validateInputs, checkRunners, and checkGovernance on the child.
func (e *Engine) workflowCallAttempt(ctx context.Context, runID string, action *workflow.Action, inputs, outputs map[string]any) attemptOutcome {
	if e.cfg.Workflows == nil {
		return attemptOutcome{attempts: 1, cerr: errf(CodeWorkflowSourceMissing,
			"action %q calls workflow %q, but no workflow source is configured on this engine", action.Name, action.Workflow)}
	}

	child, err := e.cfg.Workflows.Resolve(action.Workflow)
	if err != nil {
		return attemptOutcome{attempts: 1, cerr: asPrecondition(fmt.Errorf("resolve workflow %q: %w", action.Workflow, err))}
	}

	call := callInfoFrom(ctx)
	childDepth := call.depth + 1
	chain := append(slices.Clone(call.ancestry), child.Name)

	if childDepth > e.cfg.MaxCallDepth {
		return attemptOutcome{attempts: 1, cerr: errf(CodeCallDepthExceeded,
			"action %q exceeds the max call depth of %d: %s", action.Name, e.cfg.MaxCallDepth, strings.Join(chain, " -> "))}
	}

	childInputs, err := resolveParamsWith(action.Inputs, e.resolver(ctx, inputs, outputs))
	if err != nil {
		return attemptOutcome{attempts: 1, cerr: asPrecondition(err)}
	}

	// The child inherits the calling run's ID as its parent and the incremented
	// depth; RunWorkflow appends the child's own name to the ancestry when it
	// stamps the context its actions see.
	childCtx := withCallInfo(ctx, callInfo{parentRunID: runID, depth: childDepth, ancestry: call.ancestry})

	childRun, err := e.RunWorkflow(childCtx, child, childInputs, triggerWorkflow)
	if err != nil {
		// A rejection before any child record existed (input validation, runner
		// registration, governance) is the parent action's failure.
		return attemptOutcome{attempts: 1, cerr: asRunRejection(err)}
	}

	return e.workflowCallOutcome(ctx, childRun)
}

// workflowCallOutcome turns a finished child run into the parent action's
// outcome: a succeeded child yields the {run_id, status, outputs} output shape,
// where outputs is the child's per-action output map; a non-succeeded child
// fails the parent action with the child's canonical error code and the child
// run ID in Details.
func (e *Engine) workflowCallOutcome(ctx context.Context, child *store.Run) attemptOutcome {
	if child.Status != store.RunSucceeded {
		return attemptOutcome{attempts: 1, cerr: childFailure(child).WithDetail(detailChildRunID, child.ID)}
	}

	outputs, err := e.childOutputs(ctx, child.ID)
	if err != nil {
		return attemptOutcome{attempts: 1, cerr: errf(CodePreconditionFailed, "%v", err).WithDetail(detailChildRunID, child.ID)}
	}

	return attemptOutcome{
		attempts: 1,
		result: runner.Result{Output: map[string]any{
			fieldRunID: child.ID,
			"status":   string(child.Status),
			"outputs":  outputs,
		}},
	}
}

// childOutputs collects the child run's succeeded action outputs keyed by
// action name, the map the parent action exposes downstream as
// ${{ call.output.outputs.<action> }}.
func (e *Engine) childOutputs(ctx context.Context, childID string) (map[string]any, error) {
	results, err := e.store.ListActionResults(context.WithoutCancel(ctx), childID)
	if err != nil {
		return nil, fmt.Errorf("list child run outputs: %w", err)
	}

	outputs := make(map[string]any, len(results))

	for _, result := range results {
		if result.Status == store.ActionSucceeded {
			outputs[result.ActionName] = result.Output
		}
	}

	return outputs, nil
}

// childFailure builds the parent action's error from a failed child run,
// carrying the child's canonical code and message forward without mutating the
// child's stored error. A missing error (an unusual non-succeeded run) still
// fails the parent rather than passing silently.
func childFailure(child *store.Run) *cairnerr.Error {
	if child.Error != nil {
		return errf(child.Error.Code, "%s", child.Error.Message)
	}

	return errf(CodeRunnerPermanent, "child run %s ended %s", child.ID, child.Status)
}

// asRunRejection maps a RunWorkflow rejection (raised before the child run
// existed) to the parent action's error: a validation list surfaces its first
// code, a canonical error passes through, and anything else becomes a
// precondition failure.
func asRunRejection(err error) *cairnerr.Error {
	if list, isList := errors.AsType[cairnerr.Errors](err); isList && len(list) > 0 {
		return list[0]
	}

	return asPrecondition(err)
}
