package engine_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Approval test fixtures. The action names are named consts so the repeated
// literals do not trip goconst.
const (
	apActionDraft   = "draft"
	apActionSignoff = "signoff"
	apActionSend    = "send"
	apActorAlice    = "alice"
	apComment       = "ship it"
	apMessageWant   = "approve 3 emails?"
)

// errApprovalBoom fails the parallel branch in the failure-beats-park case.
var errApprovalBoom = errors.New("approval branch boom")

// approvalFlowTOML is a draft (task) -> signoff (approval) -> send (task) chain:
// the approval parks the run after draft, and send runs only once the approval
// resumes.
const approvalFlowTOML = `
[workflow]
name = "signoff_flow"
[action.draft]
runner = "local_fn"
params = { fn = "emit_count" }
[action.signoff]
kind = "approval"
depends_on = ["draft"]
message = "approve ${{ draft.output.email_count }} emails?"
[action.send]
runner = "local_fn"
depends_on = ["signoff"]
params = { fn = "mark" }
`

// approvalFuncs registers the local_fn helpers the approval tests use: emit_count
// yields the draft output the message reads, mark records that a downstream
// action ran, and boom fails a parallel branch.
func approvalFuncs(rec *recorder) map[string]builtins.Func {
	if rec == nil {
		rec = &recorder{}
	}

	return map[string]builtins.Func{
		"emit_count": func(_ context.Context, _ *runner.Task) (any, error) {
			return map[string]any{"email_count": float64(3)}, nil
		},
		"mark": rec.record,
		fnBoom: func(_ context.Context, _ *runner.Task) (any, error) {
			return nil, errApprovalBoom
		},
	}
}

func TestApprovalParksRunHeld(t *testing.T) {
	t.Parallel()

	eng, db := newTestEngine(t, baseConfig(), approvalFuncs(nil))

	run := mustRun(t, eng, parseFlow(t, approvalFlowTOML), nil)

	wantRunStatus(t, run, store.RunHeld)

	if !run.FinishedAt.IsZero() {
		t.Errorf("held run has finished_at %v, want zero (a parked run is not finished)", run.FinishedAt)
	}

	wantActionStatus(t, db, run.ID, apActionDraft, store.ActionSucceeded)
	wantActionStatus(t, db, run.ID, apActionSignoff, store.ActionWaiting)
	// send is blocked on the parked approval, so it never launched.
	wantActionStatus(t, db, run.ID, apActionSend, store.ActionPending)

	gate, err := db.GetApproval(t.Context(), run.ID, apActionSignoff)
	if err != nil {
		t.Fatalf("get approval: %v", err)
	}

	if gate.Status != store.ApprovalPending {
		t.Errorf("gate status = %q, want pending", gate.Status)
	}

	if gate.Message != apMessageWant {
		t.Errorf("gate message = %q, want interpolated %q", gate.Message, apMessageWant)
	}
}

func TestApprovalApproveResumesRun(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	eng, db := newTestEngine(t, baseConfig(), approvalFuncs(rec))
	flow := parseFlow(t, approvalFlowTOML)

	run := mustRun(t, eng, flow, nil)
	wantRunStatus(t, run, store.RunHeld)

	// Snapshot the pre-park succeeded draft row; resume must leave it untouched.
	draftBefore := db.storedAction(run.ID, apActionDraft)

	decide(t, db, run.ID, apActionSignoff, store.ApprovalApproved, apComment)

	resumed := mustResume(t, eng, db, run.ID, flow)
	wantRunStatus(t, resumed, store.RunSucceeded)

	signoff := wantActionStatus(t, db, run.ID, apActionSignoff, store.ActionSucceeded)

	out, isMap := signoff.Output.(map[string]any)
	if !isMap {
		t.Fatalf("signoff output = %#v, want a map", signoff.Output)
	}

	if out["approved"] != true || out["actor"] != apActorAlice || out["comment"] != apComment {
		t.Errorf("signoff output = %#v, want approved with actor %q and comment %q", out, apActorAlice, apComment)
	}

	if !slices.Contains(rec.order(), apActionSend) {
		t.Errorf("send did not run after the approval resumed; order = %v", rec.order())
	}

	if draftAfter := db.storedAction(run.ID, apActionDraft); !reflect.DeepEqual(draftBefore, draftAfter) {
		t.Errorf("resume changed the pre-park draft row:\n before %#v\n after  %#v", draftBefore, draftAfter)
	}
}

func TestApprovalDenyFailsRun(t *testing.T) {
	t.Parallel()

	eng, db := newTestEngine(t, baseConfig(), approvalFuncs(nil))
	flow := parseFlow(t, approvalFlowTOML)

	run := mustRun(t, eng, flow, nil)
	wantRunStatus(t, run, store.RunHeld)

	decide(t, db, run.ID, apActionSignoff, store.ApprovalDenied, "too risky")

	resumed := mustResume(t, eng, db, run.ID, flow)
	wantRunStatus(t, resumed, store.RunFailed)

	signoff := wantActionStatus(t, db, run.ID, apActionSignoff, store.ActionFailed)
	wantErrCode(t, signoff.Error, engine.CodeApprovalDenied)

	// The denied branch halts the run, so the downstream send is skipped.
	wantActionStatus(t, db, run.ID, apActionSend, store.ActionSkipped)
}

func TestApprovalParkWaitsForParallelBranch(t *testing.T) {
	t.Parallel()

	const flowTOML = `
[workflow]
name = "parallel_signoff"
[action.draft]
runner = "local_fn"
params = { fn = "emit_count" }
[action.signoff]
kind = "approval"
depends_on = ["draft"]
[action.sidecar]
runner = "local_fn"
params = { fn = "mark" }
`

	rec := &recorder{}
	eng, db := newTestEngine(t, baseConfig(), approvalFuncs(rec))

	run := mustRun(t, eng, parseFlow(t, flowTOML), nil)

	wantRunStatus(t, run, store.RunHeld)
	wantActionStatus(t, db, run.ID, "sidecar", store.ActionSucceeded)
	wantActionStatus(t, db, run.ID, apActionSignoff, store.ActionWaiting)

	if !slices.Contains(rec.order(), "sidecar") {
		t.Errorf("the parallel sidecar did not finish before the park; order = %v", rec.order())
	}
}

func TestApprovalTwoPendingParkTogether(t *testing.T) {
	t.Parallel()

	const flowTOML = `
[workflow]
name = "two_signoffs"
[action.sign_a]
kind = "approval"
[action.sign_b]
kind = "approval"
`

	eng, db := newTestEngine(t, baseConfig(), approvalFuncs(nil))

	run := mustRun(t, eng, parseFlow(t, flowTOML), nil)
	wantRunStatus(t, run, store.RunHeld)

	pending, err := db.ListPendingApprovals(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("list pending approvals: %v", err)
	}

	if len(pending) != 2 {
		t.Fatalf("pending approvals = %d, want 2 (parallel branches each park)", len(pending))
	}
}

func TestApprovalFailureBeatsPark(t *testing.T) {
	t.Parallel()

	const flowTOML = `
[workflow]
name = "fail_beats_park"
[action.signoff]
kind = "approval"
[action.boom]
runner = "local_fn"
params = { fn = "boom" }
`

	eng, _ := newTestEngine(t, baseConfig(), approvalFuncs(nil))

	run := mustRun(t, eng, parseFlow(t, flowTOML), nil)

	// A failed branch under halt fails the run even though another branch parked.
	wantRunStatus(t, run, store.RunFailed)
}

func TestResumeRunRejectsTerminalRun(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, baseConfig(), approvalFuncs(nil))
	flow := parseFlow(t, approvalFlowTOML)

	// A failure or cancel that beat the park leaves the gate orphaned on a run
	// that finalized terminal; a stored terminal run stands in for that shape.
	terminal := &store.Run{ID: "run-terminal", WorkflowName: flow.Name, Status: store.RunFailed}

	resumed, err := eng.ResumeRun(t.Context(), terminal, nil, flow)
	if !errors.Is(err, engine.ErrRunNotHeld) {
		t.Fatalf("ResumeRun on a %s run: err = %v, want ErrRunNotHeld", terminal.Status, err)
	}

	if resumed != nil {
		t.Errorf("ResumeRun returned %#v, want nil when it refuses a terminal run", resumed)
	}

	if terminal.Status != store.RunFailed {
		t.Errorf("ResumeRun flipped the terminal run to %s, want it left failed", terminal.Status)
	}
}

// decide records one approval decision through the store, failing the test on a
// store error so the cases stay focused on the engine behavior.
func decide(t *testing.T, db *fakeStore, runID, action string, status store.ApprovalStatus, comment string) {
	t.Helper()

	if err := db.DecideApproval(t.Context(), runID, action, status, apActorAlice, comment); err != nil {
		t.Fatalf("decide approval %s/%s: %v", runID, action, err)
	}
}

// mustResume loads the held run's rows and drives it forward, failing on a
// resume rejection.
func mustResume(t *testing.T, eng *engine.Engine, db *fakeStore, runID string, flow *workflow.Workflow) *store.Run {
	t.Helper()

	results, err := db.ListActionResults(t.Context(), runID)
	if err != nil {
		t.Fatalf("list action results: %v", err)
	}

	resumed, err := eng.ResumeRun(t.Context(), db.storedRun(runID), results, flow)
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}

	return resumed
}
