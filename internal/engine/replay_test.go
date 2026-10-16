package engine_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// replayFlowTOML is a two-stage flow: fetch succeeds and score gates on an
// external flag, so the first run fails at score and a replay reruns only score
// once the gate opens.
const replayFlowTOML = `
[workflow]
name = "replay_flow"

[action.fetch]
runner = "local_fn"
params = { fn = "fetch" }

[action.score]
runner = "local_fn"
depends_on = ["fetch"]
params = { fn = "score", value = "${{ fetch.output }}" }
`

func TestReplayRerunsFromFailedActionAndSeedsSucceeded(t *testing.T) {
	t.Parallel()

	const fetchOutput = "data"

	fetch := &countingFn{output: fetchOutput}
	score := &gateFn{}
	funcs := map[string]builtins.Func{
		"fetch": fetch.run,
		"score": score.run,
	}
	eng, db := newTestEngine(t, baseConfig(), funcs)
	flow := parseFlow(t, replayFlowTOML)

	first := mustFail(t, eng, flow)
	wantActionStatus(t, db, first.ID, "fetch", store.ActionSucceeded)
	wantActionStatus(t, db, first.ID, "score", store.ActionFailed)

	if fetch.count() != 1 {
		t.Fatalf("fetch ran %d times in the first run, want 1", fetch.count())
	}

	// Open the gate so the replayed score succeeds, then replay from the first
	// run's persisted results.
	score.open()

	priorResults, err := db.ListActionResults(t.Context(), first.ID)
	if err != nil {
		t.Fatalf("list prior results: %v", err)
	}

	replay, err := eng.ReplayRun(t.Context(), first, priorResults, flow)
	if err != nil {
		t.Fatalf("ReplayRun: %v", err)
	}

	wantRunStatus(t, replay, store.RunSucceeded)

	if replay.ParentRunID != first.ID {
		t.Errorf("replay parent_run_id = %q, want %q", replay.ParentRunID, first.ID)
	}

	if replay.Trigger != "replay" {
		t.Errorf("replay trigger = %q, want replay", replay.Trigger)
	}

	// fetch succeeded before, so the replay seeds it: the runner is never
	// re-invoked and the seeded row is stamped with the prior run.
	if fetch.count() != 1 {
		t.Fatalf("fetch re-executed on replay: %d total calls, want 1", fetch.count())
	}

	fetchRow := wantActionStatus(t, db, replay.ID, "fetch", store.ActionSucceeded)
	if fetchRow.Meta["replayed_from"] != first.ID {
		t.Errorf("seeded fetch meta[replayed_from] = %q, want %q", fetchRow.Meta["replayed_from"], first.ID)
	}

	// score failed before, so the replay reruns it: one more call than the
	// first run, and it now succeeds carrying the seeded fetch output forward.
	if score.count() != 2 {
		t.Fatalf("score ran %d times across both runs, want 2 (one failed, one replayed)", score.count())
	}

	scoreRow := wantActionStatus(t, db, replay.ID, "score", store.ActionSucceeded)
	if scoreRow.Output != fetchOutput {
		t.Errorf("replayed score output = %v, want the seeded fetch output", scoreRow.Output)
	}
}

// secretFlowTOML resolves a secret into a re-executing action's params, the
// leak-prone shape: replay re-resolves the secret and passes it to the runner
// but must persist no resolved params, so the secret never lands in a row.
const secretFlowTOML = `
[workflow]
name = "secret_flow"

[action.call]
runner = "local_fn"
params = { fn = "consume", key = "${{ secrets.api_key }}" }
`

func TestReplayPersistsNoResolvedSecret(t *testing.T) {
	t.Parallel()

	const secretValue = "sk-super-secret-value"

	cfg := baseConfig()
	cfg.Secrets = &fakeSecrets{values: map[string]string{secretAPIKey: secretValue}}

	consume := &gateFn{}
	funcs := map[string]builtins.Func{"consume": consume.run}
	eng, db := newTestEngine(t, cfg, funcs)
	flow := parseFlow(t, secretFlowTOML)

	// The first run fails at the gated action, so the replay reruns it and
	// re-resolves the secret into its params a second time.
	first := mustFail(t, eng, flow)

	consume.open()

	priorResults, err := db.ListActionResults(t.Context(), first.ID)
	if err != nil {
		t.Fatalf("list prior results: %v", err)
	}

	replay, err := eng.ReplayRun(t.Context(), first, priorResults, flow)
	if err != nil {
		t.Fatalf("ReplayRun: %v", err)
	}

	wantRunStatus(t, replay, store.RunSucceeded)

	// The secret was resolved and handed to the runner on both runs, yet no
	// persisted row may carry its plaintext: resolved params are never stored.
	assertNoSecretPersisted(t, db, first.ID, secretValue)
	assertNoSecretPersisted(t, db, replay.ID, secretValue)
}

// mustFail runs a workflow expected to end failed and returns the run.
func mustFail(t *testing.T, eng *engine.Engine, flow *workflow.Workflow) *store.Run {
	t.Helper()

	run, err := eng.RunWorkflow(t.Context(), flow, nil, triggerCLI)
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	wantRunStatus(t, run, store.RunFailed)

	return run
}

// assertNoSecretPersisted marshals a run and its action rows and fails if the
// secret plaintext appears anywhere, the grep-shaped leak check the spec asks
// for (.agents/spec/parity/workflow-language.md, "Replay from failed action").
func assertNoSecretPersisted(t *testing.T, db *fakeStore, runID, secret string) {
	t.Helper()

	run, err := db.GetRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("get run %s: %v", runID, err)
	}

	// %#v renders every field of the persisted structs, including the Inputs
	// map, action outputs, meta, and errors, so a leaked plaintext would show.
	if strings.Contains(fmt.Sprintf("%#v", run), secret) {
		t.Fatalf("run %s persisted the resolved secret", runID)
	}

	results, err := db.ListActionResults(t.Context(), runID)
	if err != nil {
		t.Fatalf("list results for %s: %v", runID, err)
	}

	for _, result := range results {
		if strings.Contains(fmt.Sprintf("%#v", result), secret) {
			t.Fatalf("action %s/%s persisted the resolved secret", runID, result.ActionName)
		}
	}
}

// countingFn is a local_fn that returns a fixed output and counts its calls, so
// a replay can prove a seeded action's runner is never re-invoked.
type countingFn struct {
	output any
	mu     sync.Mutex
	calls  int
}

func (c *countingFn) run(_ context.Context, _ *runner.Task) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.calls++

	return c.output, nil
}

func (c *countingFn) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.calls
}

// gateFn fails every call until the gate opens, then succeeds returning the
// resolved value it was handed, so a first run fails at it and a replay reruns
// it to success. It counts calls across both runs.
type gateFn struct {
	mu     sync.Mutex
	opened bool
	calls  int
}

func (g *gateFn) run(_ context.Context, task *runner.Task) (any, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.calls++

	if !g.opened {
		return nil, runner.NewError(runner.ClassPermanent, "gate.closed", "gate is closed")
	}

	return task.Params["value"], nil
}

func (g *gateFn) open() {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.opened = true
}

func (g *gateFn) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.calls
}
