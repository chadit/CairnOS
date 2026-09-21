package api_test

import (
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/auth"
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

// TestRequestRestartCountsEveryInFlightRun proves in_flight is the live count
// rather than a one-or-zero flag: three runs napping at once must all be
// reported, since that number is what an operator watches the drain against.
func TestRequestRestartCountsEveryInFlightRun(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	const napping = 3

	streams := make([]grpc.ServerStreamingClient[cairnosv1.RunEvent], 0, napping)
	runIDs := make([]string, 0, napping)

	for range napping {
		stream, err := fix.workflow.RunWorkflow(t.Context(), &cairnosv1.RunWorkflowRequest{WorkflowName: slowName})
		if err != nil {
			t.Fatalf("RunWorkflow: %v", err)
		}

		streams = append(streams, stream)
		runIDs = append(runIDs, waitForActionRunning(t, stream))
	}

	resp, err := fix.admin.RequestRestart(t.Context(), &cairnosv1.RequestRestartRequest{})
	if err != nil {
		t.Fatalf("RequestRestart: %v", err)
	}

	if resp.GetInFlight() != napping {
		t.Errorf("in_flight = %d, want %d (runs %v are still napping)", resp.GetInFlight(), napping, runIDs)
	}

	for idx, runID := range runIDs {
		if _, cancelErr := fix.workflow.CancelRun(t.Context(),
			&cairnosv1.CancelRunRequest{RunId: runID}); cancelErr != nil {
			t.Fatalf("CancelRun %s: %v", runID, cancelErr)
		}

		collectEvents(t, streams[idx])
	}
}

// TestRequestRestartDeadlineHonorsMaxWaitSeconds proves max_wait_seconds is
// the ceiling the drain actually gives up at, and that a retry asking for far
// longer cannot push that exit out.
func TestRequestRestartDeadlineHonorsMaxWaitSeconds(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	const (
		requested = 30 * time.Second
		retryWait = 3600
	)

	before := time.Now()

	if _, err := fix.admin.RequestRestart(t.Context(),
		&cairnosv1.RequestRestartRequest{MaxWaitSeconds: int64(requested / time.Second)}); err != nil {
		t.Fatalf("RequestRestart: %v", err)
	}

	after := time.Now()

	deadline := fix.drain.Deadline()
	if deadline.Before(before.Add(requested)) || deadline.After(after.Add(requested)) {
		t.Fatalf("deadline %s is outside [%s, %s]; max_wait_seconds must set it",
			deadline, before.Add(requested), after.Add(requested))
	}

	if _, err := fix.admin.RequestRestart(t.Context(),
		&cairnosv1.RequestRestartRequest{MaxWaitSeconds: retryWait}); err != nil {
		t.Fatalf("second RequestRestart: %v", err)
	}

	if got := fix.drain.Deadline(); !got.Equal(deadline) {
		t.Fatalf("deadline moved to %s from %s; a retry asking for %ds must not extend it", got, deadline, retryWait)
	}
}

// TestRequestRestartWithoutDrainIsUnimplemented covers the server built with
// no drain: it cannot exit on request, so it says so instead of accepting a
// restart it will never perform. The auth fixture wires no drain.
func TestRequestRestartWithoutDrainIsUnimplemented(t *testing.T) {
	t.Parallel()

	fix := startAuthServer(t)

	_, err := fix.admin.RequestRestart(authedCtx(t, fix.token(t, auth.ScopeAdmin)),
		&cairnosv1.RequestRestartRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("RequestRestart with no drain = %v, want Unimplemented", err)
	}

	if got := canonicalCode(t, err); got != "api.unimplemented" {
		t.Fatalf("canonical code = %q, want api.unimplemented", got)
	}
}

// TestRequestRestartNeedsAdminScope proves the restart switch is admin-only:
// a token that can run workflows cannot take the process down.
func TestRequestRestartNeedsAdminScope(t *testing.T) {
	t.Parallel()

	fix := startAuthServer(t)

	_, err := fix.admin.RequestRestart(authedCtx(t, fix.token(t, auth.ScopeWriteWorkflow)),
		&cairnosv1.RequestRestartRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("RequestRestart without the admin scope = %v, want PermissionDenied", err)
	}

	if got := canonicalCode(t, err); got != wireForbidden {
		t.Fatalf("canonical code = %q, want %q", got, wireForbidden)
	}
}
