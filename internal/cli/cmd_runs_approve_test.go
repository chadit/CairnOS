package cli_test

import (
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/store"
)

const approveWorkflow = `
[workflow]
name = "approveme"

[action.draft]
runner = "mock"
params = { output = "drafted" }

[action.signoff]
kind       = "approval"
depends_on = ["draft"]
message    = "approve the draft?"

[action.send]
runner     = "mock"
depends_on = ["signoff"]
params     = { output = "sent" }
`

const twoApprovalWorkflow = `
[workflow]
name = "twosign"

[action.sign_a]
kind = "approval"

[action.sign_b]
kind = "approval"
`

// parkedRunID runs the workflow at path and returns the single held run's id,
// failing the test unless the run parked.
func parkedRunID(t *testing.T, dir, path string) string {
	t.Helper()

	code, _, stderr := runCLI(t, runCmd, path, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("run of an approval workflow exited %d: %s", code, stderr)
	}

	db := openRunStore(t, dir)

	runs, err := db.ListRuns(t.Context(), store.ListFilter{Status: store.RunHeld})
	if err != nil || len(runs) != 1 {
		t.Fatalf("held runs: %v (%d rows)", err, len(runs))
	}

	return runs[0].ID
}

// TestRunsApproveResumesHeldRun parks a run on an approval, approves the single
// pending gate by omitting its name, and proves the run resumes to succeeded
// with the downstream action executed.
func TestRunsApproveResumesHeldRun(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, approveWorkflow)

	runID := parkedRunID(t, dir, path)

	code, stdout, stderr := runCLI(t, "runs", "approve", runID, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("approve exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "approved signoff") || !strings.Contains(stdout, "succeeded") {
		t.Fatalf("approve output missing the decision or outcome:\n%s", stdout)
	}

	db := openRunStore(t, dir)

	run, err := db.GetRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}

	if run.Status != store.RunSucceeded {
		t.Fatalf("resumed run = %s, want succeeded", run.Status)
	}
}

// TestRunsApproveDenyFailsRun parks a run, denies the gate, and proves the run
// resumes and fails the action under the denial.
func TestRunsApproveDenyFailsRun(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, approveWorkflow)

	runID := parkedRunID(t, dir, path)

	code, stdout, _ := runCLI(t, "runs", "approve", runID, "--deny", "--comment", "too risky", flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("deny exited %d, want failure for the failed run", code)
	}

	if !strings.Contains(stdout, "denied signoff") {
		t.Fatalf("deny output missing the decision:\n%s", stdout)
	}

	db := openRunStore(t, dir)

	run, err := db.GetRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}

	if run.Status != store.RunFailed {
		t.Fatalf("denied run = %s, want failed", run.Status)
	}
}

// TestRunsApproveRejectsTerminalRun proves a decision on a gate whose run
// already finished is refused before any decision write, so a failure or cancel
// that beat the park cannot be resurrected through the approve path.
func TestRunsApproveRejectsTerminalRun(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, approveWorkflow)

	runID := parkedRunID(t, dir, path)

	// Force the parked run terminal to mimic a failure or cancel that beat the
	// approval branch, leaving the pending gate orphaned on a failed run.
	db := openRunStore(t, dir)
	if err := db.UpdateRunStatus(t.Context(), runID, store.RunFailed, nil); err != nil {
		t.Fatalf("force run failed: %v", err)
	}

	code, _, stderr := runCLI(t, "runs", "approve", runID, flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("approve of a terminal run exited %d, want failure", code)
	}

	if !strings.Contains(stderr, "not awaiting approval") {
		t.Fatalf("approve error does not explain the refusal:\n%s", stderr)
	}

	run, err := db.GetRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}

	if run.Status != store.RunFailed {
		t.Fatalf("terminal run resumed to %s, want it left failed", run.Status)
	}

	// The gate stays pending because the refusal lands before any decision write.
	pending, err := db.ListPendingApprovals(t.Context(), runID)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}

	if len(pending) != 1 {
		t.Fatalf("pending approvals = %d, want the gate left untouched at 1", len(pending))
	}
}

// TestRunsApproveAmbiguousRequiresName parks a run with two pending gates: a
// name-omitted decision errors listing both action names, and naming one lands
// on exactly that gate.
func TestRunsApproveAmbiguousRequiresName(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	path := writeWorkflow(t, dir, twoApprovalWorkflow)

	runID := parkedRunID(t, dir, path)

	code, _, stderr := runCLI(t, "runs", "approve", runID, flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("ambiguous approve exited %d, want failure", code)
	}

	if !strings.Contains(stderr, "sign_a") || !strings.Contains(stderr, "sign_b") {
		t.Fatalf("ambiguous error does not list the pending gates:\n%s", stderr)
	}

	// Naming one gate decides it; the run parks again on the other.
	code, _, stderr = runCLI(t, "runs", "approve", runID, "sign_a", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("named approve exited %d: %s", code, stderr)
	}

	db := openRunStore(t, dir)

	pending, err := db.ListPendingApprovals(t.Context(), runID)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}

	if len(pending) != 1 || pending[0].ActionName != "sign_b" {
		t.Fatalf("pending after one decision = %+v, want only sign_b", pending)
	}
}
