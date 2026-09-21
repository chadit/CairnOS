package api_test

// ApproveRun over the API: park a run on an approval gate, decide it over
// gRPC, and prove the run resumes in the serve process
// (.agents/spec/parity/workflow-language.md, "Human approval").

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/store"
)

// Statuses the approval assertions wait on; the shared block in api_test.go
// only names the ones its own tests use.
const (
	statusHeld   = "held"
	statusFailed = "failed"
)

// gatedTOML parks between a draft and a send, the CLI approve fixture's
// shape, so the resume proves downstream work runs after the decision.
const gatedTOML = `
[workflow]
name = "gated"

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

// The two parallel gates twoGateTOML declares, named by the tests that decide
// them one at a time.
const (
	actionSignA = "sign_a"
	actionSignB = "sign_b"
)

// twoGateTOML parks on two parallel approvals, making the omitted-name
// request ambiguous.
const twoGateTOML = `
[workflow]
name = "twogate"

[action.sign_a]
kind = "approval"

[action.sign_b]
kind = "approval"
`

// parkHeldRun runs a workflow expected to park and returns the held run's id
// from the stream's terminal event.
func parkHeldRun(t *testing.T, fix *fixture, workflowName string) string {
	t.Helper()

	stream, err := fix.workflow.RunWorkflow(t.Context(), &cairnosv1.RunWorkflowRequest{WorkflowName: workflowName})
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	events := collectEvents(t, stream)
	last := events[len(events)-1]

	if got := last.GetRun().GetStatus(); got != statusHeld {
		t.Fatalf("approval run parked as %q, want %q", got, statusHeld)
	}

	return last.GetRun().GetRunId()
}

// waitApproveFor polls condition until it holds or the deadline passes. The
// resume after an ApproveRun ack runs on real server goroutines, so the wait
// ticks off a real timer rather than synthetic time, the bridge tests'
// waitFor shape.
func waitApproveFor(t *testing.T, describe string, condition func() bool) {
	t.Helper()

	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		if condition() {
			return
		}

		select {
		case <-timeout.C:
			t.Fatalf("%s: condition not met before deadline", describe)
		case <-ticker.C:
		}
	}
}

// getRun fetches the run over the API, failing the test on transport errors.
func getRun(t *testing.T, fix *fixture, runID string) *cairnosv1.GetRunResponse {
	t.Helper()

	resp, err := fix.workflow.GetRun(t.Context(), &cairnosv1.GetRunRequest{RunId: runID})
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	return resp
}

// actionStatus reports the status recorded for one action of a run, or the
// empty string when the run carries no result for that name yet.
func actionStatus(resp *cairnosv1.GetRunResponse, name string) string {
	for _, action := range resp.GetActions() {
		if action.GetName() == name {
			return action.GetStatus()
		}
	}

	return ""
}

// waitForRunStatus polls GetRun until the run reaches want; the terminal
// state of a resumed run arrives eventually, not on the ack.
func waitForRunStatus(t *testing.T, fix *fixture, runID, want string) *cairnosv1.GetRunResponse {
	t.Helper()

	waitApproveFor(t, "run "+runID+" reaching "+want, func() bool {
		return getRun(t, fix, runID).GetRun().GetStatus() == want
	})

	return getRun(t, fix, runID)
}

// TestApproveRunResumesHeldRun parks a run on its gate, approves it with the
// action name omitted, and proves the serve process resumes the run to
// succeeded with the downstream action executed.
func TestApproveRunResumesHeldRun(t *testing.T) {
	t.Parallel()

	fix := startServer(t)
	writeWorkflow(t, fix.workflowsDir, "gated.toml", gatedTOML)

	runID := parkHeldRun(t, fix, "gated")

	ack, err := fix.workflow.ApproveRun(t.Context(), &cairnosv1.ApproveRunRequest{RunId: runID, Approve: true})
	if err != nil {
		t.Fatalf("ApproveRun: %v", err)
	}

	if ack.GetActionName() != "signoff" || !ack.GetApproved() || ack.GetStatus() != "resuming" {
		t.Fatalf("ack = %q/%v/%q, want signoff/true/resuming",
			ack.GetActionName(), ack.GetApproved(), ack.GetStatus())
	}

	final := waitForRunStatus(t, fix, runID, statusSucceeded)

	var sent bool

	for _, action := range final.GetActions() {
		if action.GetName() == "send" && action.GetStatus() == statusSucceeded {
			sent = true
		}
	}

	if !sent {
		t.Fatalf("downstream send action did not succeed after approval: %v", final.GetActions())
	}
}

// TestApproveRunDenyFailsRun parks a run, denies the gate with a comment,
// and proves the resumed run fails under the denial.
func TestApproveRunDenyFailsRun(t *testing.T) {
	t.Parallel()

	fix := startServer(t)
	writeWorkflow(t, fix.workflowsDir, "gated.toml", gatedTOML)

	runID := parkHeldRun(t, fix, "gated")

	ack, err := fix.workflow.ApproveRun(t.Context(), &cairnosv1.ApproveRunRequest{
		RunId:   runID,
		Approve: false,
		Comment: "too risky",
	})
	if err != nil {
		t.Fatalf("ApproveRun deny: %v", err)
	}

	if ack.GetApproved() {
		t.Fatalf("deny ack reports approved")
	}

	waitForRunStatus(t, fix, runID, statusFailed)
}

// TestApproveRunRefusesTerminalRun proves a decision on a run that already
// finished is refused before any decision write, so a terminal run is never
// resurrected through the approve path.
func TestApproveRunRefusesTerminalRun(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	runID := "finished-run"
	if err := fix.db.CreateRun(t.Context(), &store.Run{
		ID:           runID,
		WorkflowName: helloName,
		WorkflowHash: "deadbeef",
		Status:       store.RunSucceeded,
		Trigger:      "cli",
		CreatedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("seed finished run: %v", err)
	}

	_, err := fix.workflow.ApproveRun(t.Context(), &cairnosv1.ApproveRunRequest{RunId: runID, Approve: true})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("approve of a finished run code = %v (%v), want FailedPrecondition", status.Code(err), err)
	}
}

// TestApproveRunUnknownRunNotFound proves an unknown run id answers NotFound.
func TestApproveRunUnknownRunNotFound(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	_, err := fix.workflow.ApproveRun(t.Context(), &cairnosv1.ApproveRunRequest{RunId: "no-such-run", Approve: true})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("approve of an unknown run code = %v (%v), want NotFound", status.Code(err), err)
	}
}

// TestApproveRunUnknownActionNotFound proves a decision aimed at an action
// that is not a pending gate is refused as not-found, so a typo in the name
// cannot read as a server fault and cannot settle some other gate.
func TestApproveRunUnknownActionNotFound(t *testing.T) {
	t.Parallel()

	fix := startServer(t)
	writeWorkflow(t, fix.workflowsDir, "gated.toml", gatedTOML)

	runID := parkHeldRun(t, fix, "gated")

	_, err := fix.workflow.ApproveRun(t.Context(), &cairnosv1.ApproveRunRequest{
		RunId:      runID,
		ActionName: "no-such-gate",
		Approve:    true,
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("approve of an unknown action code = %v (%v), want NotFound", status.Code(err), err)
	}

	// The real gate is untouched, so the run is still parked on it.
	pending, err := fix.db.ListPendingApprovals(t.Context(), runID)
	if err != nil {
		t.Fatalf("list pending approvals: %v", err)
	}

	if len(pending) != 1 || pending[0].ActionName != "signoff" {
		t.Fatalf("pending gates after the bad name = %+v, want signoff still pending", pending)
	}
}

// TestApproveRunRefusesDecidedGate proves a second decision on a gate that is
// already settled is refused, so a retry cannot overwrite the first decision's
// actor and comment while the run parks on its next gate.
func TestApproveRunRefusesDecidedGate(t *testing.T) {
	t.Parallel()

	fix := startServer(t)
	writeWorkflow(t, fix.workflowsDir, "twogate.toml", twoGateTOML)

	runID := parkHeldRun(t, fix, "twogate")

	if _, err := fix.workflow.ApproveRun(t.Context(), &cairnosv1.ApproveRunRequest{
		RunId:      runID,
		ActionName: actionSignA,
		Approve:    true,
	}); err != nil {
		t.Fatalf("approve sign_a: %v", err)
	}

	// sign_a settles during the resume, and the run then re-parks on sign_b.
	// Reading sign_a first keeps a stale held from passing for the second park.
	waitApproveFor(t, "run re-parking on sign_b", func() bool {
		if actionStatus(getRun(t, fix, runID), actionSignA) != statusSucceeded {
			return false
		}

		return getRun(t, fix, runID).GetRun().GetStatus() == statusHeld
	})

	_, err := fix.workflow.ApproveRun(t.Context(), &cairnosv1.ApproveRunRequest{
		RunId:      runID,
		ActionName: actionSignA,
		Approve:    false,
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("second decision on sign_a code = %v (%v), want NotFound", status.Code(err), err)
	}

	decided, err := fix.db.GetApproval(t.Context(), runID, actionSignA)
	if err != nil {
		t.Fatalf("get sign_a approval: %v", err)
	}

	if decided.Status != store.ApprovalApproved {
		t.Fatalf("sign_a status = %q, want the first decision %q to stand", decided.Status, store.ApprovalApproved)
	}
}

// TestApproveRunAmbiguousRequiresName parks a run holding two pending gates,
// proves the omitted-name request is refused listing both, then decides each
// gate by name through the re-park to completion.
func TestApproveRunAmbiguousRequiresName(t *testing.T) {
	t.Parallel()

	fix := startServer(t)
	writeWorkflow(t, fix.workflowsDir, "twogate.toml", twoGateTOML)

	runID := parkHeldRun(t, fix, "twogate")

	_, err := fix.workflow.ApproveRun(t.Context(), &cairnosv1.ApproveRunRequest{RunId: runID, Approve: true})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ambiguous approve code = %v (%v), want FailedPrecondition", status.Code(err), err)
	}

	if msg := status.Convert(err).Message(); !strings.Contains(msg, actionSignA) || !strings.Contains(msg, actionSignB) {
		t.Fatalf("ambiguous refusal does not list the pending gates: %s", msg)
	}

	if _, approveRunErr := fix.workflow.ApproveRun(t.Context(), &cairnosv1.ApproveRunRequest{
		RunId:      runID,
		ActionName: actionSignA,
		Approve:    true,
	}); approveRunErr != nil {
		t.Fatalf("approve sign_a: %v", approveRunErr)
	}

	// Both parks read held, so approving sign_b against the stale first-park read
	// is refused. sign_a reaches succeeded only during the resume, so a held seen
	// after that is the second park. GetRun reads the run row before the action
	// rows and can pair a stale held with a settled sign_a, so these stay two
	// ordered reads.
	waitApproveFor(t, "run re-parking on sign_b", func() bool {
		if actionStatus(getRun(t, fix, runID), actionSignA) != statusSucceeded {
			return false
		}

		return getRun(t, fix, runID).GetRun().GetStatus() == statusHeld
	})

	pending, err := fix.db.ListPendingApprovals(t.Context(), runID)
	if err != nil {
		t.Fatalf("list pending approvals: %v", err)
	}

	if len(pending) != 1 || pending[0].ActionName != actionSignB {
		t.Fatalf("pending gates after deciding sign_a = %+v, want only sign_b", pending)
	}

	if _, err := fix.workflow.ApproveRun(t.Context(), &cairnosv1.ApproveRunRequest{
		RunId:      runID,
		ActionName: actionSignB,
		Approve:    true,
	}); err != nil {
		t.Fatalf("approve sign_b: %v", err)
	}

	waitForRunStatus(t, fix, runID, statusSucceeded)
}
