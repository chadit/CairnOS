package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
)

// triggerReplay marks a run created by replaying a prior one, so run history
// tells a replay apart from the original run and its other triggers.
const triggerReplay = "replay"

// metaReplayedFrom stamps a replayed action's persisted row with the prior run
// its output was carried from, so history shows which stage re-used an old
// result instead of re-executing (.agents/spec/parity/workflow-language.md,
// "Replay from failed action").
const metaReplayedFrom = "replayed_from"

// ReplayRun starts a fresh run seeded from a prior one: every action the prior
// run finished successfully carries its output forward as a persisted seed row
// stamped replayed_from, and the first failed action plus everything
// downstream re-executes through the normal loop under the current runner and
// governance checks (.agents/spec/parity/workflow-language.md, "Replay from
// failed action"). No resolved params are persisted, so a replay never writes
// a secret to the store: it reuses the prior run's inputs (already durable in
// runs.inputs_json) and upstream outputs (already durable in
// action_results.output_json), which is exactly what re-resolution needs.
//
// The returned run is terminal; its Status and Error carry the outcome, so a
// failed replay returns a nil error here. A non-nil error means the replay was
// rejected before any record was created.
func (e *Engine) ReplayRun(ctx context.Context, prior *store.Run, priorResults []*store.ActionResult, flow *workflow.Workflow) (*store.Run, error) {
	if flow == nil {
		return nil, ErrNilWorkflow
	}

	if prior == nil {
		return nil, ErrNilRun
	}

	// The prior run's inputs are the already-validated set (engine.RunWorkflow
	// stores run.Inputs = validated), so they need no re-validation; re-running
	// checkRunners and checkGovernance re-applies the run-start gates against
	// the current file and the current budget/pause state.
	inputs := prior.Inputs

	if err := e.checkRunners(flow, inputs); err != nil {
		return nil, err
	}

	if err := e.checkGovernance(ctx, flow); err != nil {
		return nil, err
	}

	run := &store.Run{
		ID:           e.cfg.NewID(),
		WorkflowName: flow.Name,
		WorkflowHash: flow.Hash,
		WorkspaceID:  store.WorkspaceFromContext(ctx),
		Status:       store.RunPending,
		Inputs:       inputs,
		Trigger:      triggerReplay,
		ParentRunID:  prior.ID,
		CreatedAt:    time.Now(),
	}

	if err := e.store.CreateRun(ctx, run); err != nil {
		return nil, fmt.Errorf("create replay run: %w", err)
	}

	e.executeSeededDAG(ctx, flow, run, inputs, succeededSeeds(flow, priorResults), true)

	return run, nil
}

// succeededSeeds selects the prior run's succeeded actions whose output carries
// forward, the seed set both a replay (new run) and a resume (held run) reuse.
// Only succeeded actions seed: failed, skipped, and never-started actions
// re-execute through the loop. An action absent from the current workflow is
// dropped, so a replay after an edit that removed a stage does not seed one the
// new DAG has no place for.
func succeededSeeds(flow *workflow.Workflow, priorResults []*store.ActionResult) map[string]seedEntry {
	seeds := make(map[string]seedEntry, len(priorResults))

	for _, result := range priorResults {
		if result.Status != store.ActionSucceeded {
			continue
		}

		if _, defined := flow.Actions[result.ActionName]; !defined {
			continue
		}

		seeds[result.ActionName] = seedEntry{Output: result.Output}
	}

	return seeds
}
