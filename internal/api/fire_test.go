package api_test

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/scheduler"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/sqlite"
)

// FireRunner test fixtures, named per the repo's goconst-in-tests policy.
const (
	fireFlowName = "fire_flow"
	fireTagKey   = "tag"
	fireTagValue = "poll-value"
	fireSchedule = "s1"
)

// fireWorkflow declares one optional input referenced by the action, so Fire
// (no inputs) runs on the default and FireRun overrides it.
const fireWorkflow = `
[workflow]
name = "fire_flow"

[workflow.inputs.tag]
type    = "string"
default = "default-tag"

[action.tick]
runner = "mock"
params = { output = "${{ inputs.tag }}" }
`

// newFireRunner wires a FireRunner over a real sqlite store, the mock runner, and
// a fixture workflow on disk, returning the runner, the store to read runs back,
// and the workflow path.
func newFireRunner(t *testing.T) (*api.FireRunner, *sqlite.Store, string) {
	t.Helper()

	dir := t.TempDir()

	db, err := sqlite.Open(filepath.Join(dir, "cairnos.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.DiscardHandler)
	manager := api.NewManager(logger)
	manager.Bind(engine.New(testEngineConfig(manager), testRegistry(t), db))

	writeWorkflow(t, dir, fireFlowName+".toml", fireWorkflow)

	return api.NewFireRunner(manager, nil, logger), db, filepath.Join(dir, fireFlowName+".toml")
}

// latestRun returns the newest run for a workflow, failing when there is none.
func latestRun(t *testing.T, db store.Store, workflowName string) *store.Run {
	t.Helper()

	runs, err := db.ListRuns(t.Context(), store.ListFilter{WorkflowName: workflowName})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}

	if len(runs) == 0 {
		t.Fatalf("no runs for %s", workflowName)
	}

	return runs[0]
}

// TestFireRunnerFireStampsScheduleTrigger proves the base Runner path is
// unchanged: Fire runs the workflow with default inputs and trigger "schedule".
func TestFireRunnerFireStampsScheduleTrigger(t *testing.T) {
	t.Parallel()

	runner, db, path := newFireRunner(t)
	sched := &scheduler.Schedule{ID: fireSchedule, WorkflowPath: path, WorkflowName: fireFlowName}

	if err := runner.Fire(t.Context(), sched); err != nil {
		t.Fatalf("fire: %v", err)
	}

	run := latestRun(t, db, fireFlowName)
	if run.Trigger != "schedule" {
		t.Errorf("Fire trigger = %q, want schedule", run.Trigger)
	}
}

// TestFireRunnerFireRunCarriesInputsAndPollTrigger proves the poll path: FireRun
// reaches the manager with the caller's inputs and trigger "poll", and mints a
// distinct run from a plain scheduled fire.
func TestFireRunnerFireRunCarriesInputsAndPollTrigger(t *testing.T) {
	t.Parallel()

	runner, db, path := newFireRunner(t)
	sched := &scheduler.Schedule{ID: fireSchedule, WorkflowPath: path, WorkflowName: fireFlowName}

	if err := runner.Fire(t.Context(), sched); err != nil {
		t.Fatalf("seed scheduled run: %v", err)
	}

	scheduled := latestRun(t, db, fireFlowName)

	if err := runner.FireRun(t.Context(), sched, map[string]any{fireTagKey: fireTagValue}, "poll"); err != nil {
		t.Fatalf("fire run: %v", err)
	}

	polled := latestRun(t, db, fireFlowName)

	if polled.Trigger != "poll" {
		t.Errorf("FireRun trigger = %q, want poll", polled.Trigger)
	}

	if polled.Inputs[fireTagKey] != fireTagValue {
		t.Errorf("FireRun inputs = %v, want %s=%s", polled.Inputs, fireTagKey, fireTagValue)
	}

	if polled.ID == scheduled.ID {
		t.Error("FireRun reused the scheduled run id; want a distinct run")
	}
}
