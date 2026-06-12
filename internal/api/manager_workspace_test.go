package api_test

import (
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/auth"
)

// workspace ids the cross-tenant manager tests run under.
const (
	tenantA = "tenant-a"
	tenantB = "tenant-b"
)

// TestManagerRefusesCrossTenantCancel proves the run manager carries the
// workspace into its per-run state: tenant B cannot cancel tenant A's active
// run by id. CancelRun reads as NotFound (the store's own answer, no oracle),
// TerminateRun reads as FailedPrecondition without canceling, and the run keeps
// running. Same-tenant cancel still works and finalizes the run as canceled.
func TestManagerRefusesCrossTenantCancel(t *testing.T) {
	t.Parallel()

	fix := startAuthServer(t)

	tokenA := fix.tokenInWorkspace(t, tenantA, auth.ScopeAdmin)
	tokenB := fix.tokenInWorkspace(t, tenantB, auth.ScopeAdmin)

	// Tenant A starts a long-lived run; the stream lets the test wait for the
	// action to be running and drain to the terminal event after cancel.
	stream, err := fix.workflow.RunWorkflow(authedCtx(t, tokenA),
		&cairnosv1.RunWorkflowRequest{WorkflowName: slowName})
	if err != nil {
		t.Fatalf("tenant A RunWorkflow: %v", err)
	}

	runID := waitForActionRunning(t, stream)

	// Tenant B cannot cancel tenant A's run by id: the store path answers
	// NotFound, the same answer an unknown id gives.
	_, err = fix.workflow.CancelRun(authedCtx(t, tokenB), &cairnosv1.CancelRunRequest{RunId: runID})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("tenant B CancelRun(A's run) code = %v, want NotFound", status.Code(err))
	}

	// Tenant B cannot terminate it either: the manager refuses the foreign
	// workspace, so TerminateRun reports the run is not active here.
	_, err = fix.governance.TerminateRun(authedCtx(t, tokenB), &cairnosv1.TerminateRunRequest{RunId: runID})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("tenant B TerminateRun(A's run) code = %v, want FailedPrecondition", status.Code(err))
	}

	// The run kept running through both foreign attempts.
	got, err := fix.workflow.GetRun(authedCtx(t, tokenA), &cairnosv1.GetRunRequest{RunId: runID})
	if err != nil {
		t.Fatalf("tenant A GetRun: %v", err)
	}

	if gotStatus := got.GetRun().GetStatus(); gotStatus != statusRunning {
		t.Fatalf("run status after cross-tenant attempts = %q, want %q", gotStatus, statusRunning)
	}

	// Same-tenant cancel still works: tenant A stops its own run.
	ack, err := fix.workflow.CancelRun(authedCtx(t, tokenA), &cairnosv1.CancelRunRequest{RunId: runID})
	if err != nil {
		t.Fatalf("tenant A CancelRun(own run): %v", err)
	}

	if ack.GetRunId() != runID {
		t.Fatalf("cancel ack run_id = %q, want %q", ack.GetRunId(), runID)
	}

	assertStreamCanceled(t, stream)
}

// assertStreamCanceled drains the run event stream to its terminal event and
// checks the run finalized as canceled, the event-driven proof that the
// same-tenant cancel landed without any clock wait.
func assertStreamCanceled(t *testing.T, stream grpc.ServerStreamingClient[cairnosv1.RunEvent]) {
	t.Helper()

	events := collectEvents(t, stream)
	if len(events) == 0 {
		t.Fatal("no events after cancel; want the stream to finish with run_finished")
	}

	last := events[len(events)-1]
	if got := last.GetRun().GetStatus(); got != statusCanceled {
		t.Fatalf("final run status = %q, want %q", got, statusCanceled)
	}
}
