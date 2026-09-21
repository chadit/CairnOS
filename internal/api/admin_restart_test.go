package api_test

import (
	"testing"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
)

// TestRequestRestartCountsInFlightRunsAndStartsDraining holds one run open,
// asks for a restart, and checks both halves of the answer: the count an
// operator reads to know what the drain is waiting on, and the flag the serve
// wiring gates its scheduler on.
func TestRequestRestartCountsInFlightRunsAndStartsDraining(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	if fix.drain.Draining() {
		t.Fatal("a fresh server must not be draining")
	}

	stream, err := fix.workflow.RunWorkflow(t.Context(), &cairnosv1.RunWorkflowRequest{WorkflowName: slowName})
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	runID := waitForActionRunning(t, stream)

	resp, err := fix.admin.RequestRestart(t.Context(), &cairnosv1.RequestRestartRequest{})
	if err != nil {
		t.Fatalf("RequestRestart: %v", err)
	}

	if !resp.GetAccepted() {
		t.Error("RequestRestart must accept the request")
	}

	if resp.GetInFlight() != 1 {
		t.Errorf("in_flight = %d, want 1 (run %s is still napping)", resp.GetInFlight(), runID)
	}

	if !fix.drain.Draining() {
		t.Error("RequestRestart must put the server into draining")
	}

	deadline := fix.drain.Deadline()

	second, err := fix.admin.RequestRestart(t.Context(), &cairnosv1.RequestRestartRequest{MaxWaitSeconds: 1})
	if err != nil {
		t.Fatalf("second RequestRestart: %v", err)
	}

	if !second.GetAccepted() {
		t.Error("a second request while draining must report the state, not fail")
	}

	if got := fix.drain.Deadline(); !got.Equal(deadline) {
		t.Errorf("deadline moved to %s from %s; a retry must not extend the drain", got, deadline)
	}

	if _, cancelErr := fix.workflow.CancelRun(t.Context(),
		&cairnosv1.CancelRunRequest{RunId: runID}); cancelErr != nil {
		t.Fatalf("CancelRun: %v", cancelErr)
	}

	collectEvents(t, stream)
}

// TestRequestRestartWithNoRunsReportsZero proves the idle answer is a real
// count rather than a placeholder, since zero is what tells the caller the
// process exits right away.
func TestRequestRestartWithNoRunsReportsZero(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	resp, err := fix.admin.RequestRestart(t.Context(), &cairnosv1.RequestRestartRequest{})
	if err != nil {
		t.Fatalf("RequestRestart: %v", err)
	}

	if resp.GetInFlight() != 0 {
		t.Errorf("in_flight = %d, want 0 on an idle server", resp.GetInFlight())
	}

	if !fix.drain.Draining() {
		t.Error("an idle server must still enter draining so the watcher exits")
	}
}
