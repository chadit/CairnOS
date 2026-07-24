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

	if msg := status.Convert(err).Message(); !strings.Contains(msg, "sign_a") || !strings.Contains(msg, "sign_b") {
		t.Fatalf("ambiguous refusal does not list the pending gates: %s", msg)
	}

	if _, err := fix.workflow.ApproveRun(t.Context(), &cairnosv1.ApproveRunRequest{
		RunId:      runID,
		ActionName: "sign_a",
		Approve:    true,
	}); err != nil {
		t.Fatalf("approve sign_a: %v", err)
	}

	// The resumed run settles sign_a and re-parks held on sign_b; one pending
	// gate left is the observable boundary between the two parks.
	waitApproveFor(t, "sign_a gate settling", func() bool {
		pending, err := fix.db.ListPendingApprovals(t.Context(), runID)

		return err == nil && len(pending) == 1
	})
	waitForRunStatus(t, fix, runID, statusHeld)

	if _, err := fix.workflow.ApproveRun(t.Context(), &cairnosv1.ApproveRunRequest{
		RunId:      runID,
		ActionName: "sign_b",
		Approve:    true,
	}); err != nil {
		t.Fatalf("approve sign_b: %v", err)
	}

	waitForRunStatus(t, fix, runID, statusSucceeded)
}
