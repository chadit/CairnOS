package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/store"
)

// Approval conformance fixtures; goconst applies to library code in this
// repo's lint policy, so the repeated literals are named.
const (
	approvalActor      = "alice"
	approvalOtherActor = "mallory"
	approvalComment    = "looks good"
	approvalMessage    = "approve sending 3 emails?"
)

// testApprovalRoundTrip proves a pending gate persists with its message, reads
// back, and moves once to a decided state carrying the actor and comment
// (.agents/spec/parity/workflow-language.md, "Human approval").
func testApprovalRoundTrip(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	created := time.Date(2026, 6, 20, 9, 0, 0, microFixtureNS, time.UTC)

	runID := store.NewID()
	createRun(t, db, runID, workflowAlpha, store.RunHeld, created)

	err := db.CreateApproval(ctx, &store.Approval{
		RunID:      runID,
		ActionName: actionFetch,
		Message:    approvalMessage,
		CreatedAt:  created,
	})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}

	got, err := db.GetApproval(ctx, runID, actionFetch)
	if err != nil {
		t.Fatalf("get approval: %v", err)
	}

	if got.Status != store.ApprovalPending {
		t.Errorf("status = %q, want pending", got.Status)
	}

	if got.Message != approvalMessage {
		t.Errorf("message = %q, want %q", got.Message, approvalMessage)
	}

	if got.WorkspaceID != store.DefaultWorkspace {
		t.Errorf("workspace = %q, want default", got.WorkspaceID)
	}

	if !got.CreatedAt.Equal(created) {
		t.Errorf("created_at = %v, want %v", got.CreatedAt, created)
	}

	if !got.DecidedAt.IsZero() {
		t.Errorf("pending approval has decided_at %v", got.DecidedAt)
	}

	if decideApprovalErr := db.DecideApproval(ctx, runID, actionFetch, store.ApprovalApproved, approvalActor, approvalComment); decideApprovalErr != nil {
		t.Fatalf("decide approval: %v", decideApprovalErr)
	}

	decided, err := db.GetApproval(ctx, runID, actionFetch)
	if err != nil {
		t.Fatalf("get decided approval: %v", err)
	}

	if decided.Status != store.ApprovalApproved {
		t.Errorf("decided status = %q, want approved", decided.Status)
	}

	if decided.Actor != approvalActor || decided.Comment != approvalComment {
		t.Errorf("decided actor/comment = %q/%q, want %q/%q",
			decided.Actor, decided.Comment, approvalActor, approvalComment)
	}

	if decided.DecidedAt.IsZero() {
		t.Error("decided approval has no decided_at stamp")
	}
}

// testApprovalNotFound proves GetApproval and DecideApproval report the
// not-found sentinel for a gate that has no pending row, so callers classify
// it instead of seeing a silent no-op.
func testApprovalNotFound(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	if _, err := db.GetApproval(ctx, "missing", actionFetch); !errors.Is(err, store.ErrApprovalNotFound) {
		t.Errorf("GetApproval error = %v, want ErrApprovalNotFound", err)
	}

	err := db.DecideApproval(ctx, "missing", actionFetch, store.ApprovalApproved, approvalActor, "")
	if !errors.Is(err, store.ErrApprovalNotFound) {
		t.Errorf("DecideApproval error = %v, want ErrApprovalNotFound", err)
	}
}

// testApprovalDecideOnce proves a gate moves exactly once: a second decision
// on an already-decided gate reads as not-found, so a late denial cannot
// overturn an approval.
func testApprovalDecideOnce(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	runID := store.NewID()
	createRun(t, db, runID, workflowAlpha, store.RunHeld, time.Now())
	seedApproval(t, db, ctx, runID, actionFetch)

	if err := db.DecideApproval(ctx, runID, actionFetch, store.ApprovalApproved, approvalActor, ""); err != nil {
		t.Fatalf("first decide: %v", err)
	}

	err := db.DecideApproval(ctx, runID, actionFetch, store.ApprovalDenied, approvalOtherActor, "")
	if !errors.Is(err, store.ErrApprovalNotFound) {
		t.Fatalf("second decide error = %v, want ErrApprovalNotFound", err)
	}

	got, err := db.GetApproval(ctx, runID, actionFetch)
	if err != nil {
		t.Fatalf("get approval: %v", err)
	}

	if got.Status != store.ApprovalApproved || got.Actor != approvalActor {
		t.Errorf("gate = %s by %q, want approved by %q; the second decision overwrote it",
			got.Status, got.Actor, approvalActor)
	}
}

// testApprovalListPending proves ListPendingApprovals returns a run's pending
// gates ordered by action name and drops a gate once it is decided, the
// listing the name-omitted decision path relies on.
func testApprovalListPending(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	runID := store.NewID()
	createRun(t, db, runID, workflowAlpha, store.RunHeld, time.Now())

	// actionBrain sorts before actionFetch, so the listing proves the order.
	seedApproval(t, db, ctx, runID, actionFetch)
	seedApproval(t, db, ctx, runID, actionBrain)

	pending, err := db.ListPendingApprovals(ctx, runID)
	if err != nil {
		t.Fatalf("list pending approvals: %v", err)
	}

	if len(pending) != twoUnits {
		t.Fatalf("listed %d pending approvals, want 2", len(pending))
	}

	if pending[0].ActionName != actionBrain || pending[1].ActionName != actionFetch {
		t.Errorf("pending order = %q,%q, want %q,%q",
			pending[0].ActionName, pending[1].ActionName, actionBrain, actionFetch)
	}

	if decideApprovalErr := db.DecideApproval(ctx, runID, actionBrain, store.ApprovalApproved, approvalActor, ""); decideApprovalErr != nil {
		t.Fatalf("decide brain: %v", decideApprovalErr)
	}

	remaining, err := db.ListPendingApprovals(ctx, runID)
	if err != nil {
		t.Fatalf("list pending after decide: %v", err)
	}

	if len(remaining) != 1 || remaining[0].ActionName != actionFetch {
		t.Errorf("remaining pending = %v, want only %s", approvalActionNames(remaining), actionFetch)
	}
}

// testApprovalIsolatesByWorkspace proves a gate created in one workspace is
// invisible to another: a cross-tenant read, decision, or listing never
// touches it (.agents/spec/12-ui-and-governance.md, "Multi-tenancy").
func testApprovalIsolatesByWorkspace(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	base := time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC)

	acmeCtx := store.ContextWithWorkspace(t.Context(), workspaceAcme)
	globexCtx := store.ContextWithWorkspace(t.Context(), workspaceGlobex)

	acmeRun := store.NewID()
	createRunIn(t, db, acmeCtx, acmeRun, base)
	seedApproval(t, db, acmeCtx, acmeRun, actionFetch)

	if _, err := db.GetApproval(globexCtx, acmeRun, actionFetch); !errors.Is(err, store.ErrApprovalNotFound) {
		t.Fatalf("globex GetApproval(acme gate) error = %v, want ErrApprovalNotFound", err)
	}

	decideErr := db.DecideApproval(globexCtx, acmeRun, actionFetch, store.ApprovalApproved, approvalOtherActor, "")
	if !errors.Is(decideErr, store.ErrApprovalNotFound) {
		t.Fatalf("globex DecideApproval(acme gate) error = %v, want ErrApprovalNotFound", decideErr)
	}

	globexPending, err := db.ListPendingApprovals(globexCtx, acmeRun)
	if err != nil {
		t.Fatalf("globex ListPendingApprovals: %v", err)
	}

	if len(globexPending) != 0 {
		t.Fatalf("globex saw %d of acme's approvals, want 0", len(globexPending))
	}

	own, err := db.GetApproval(acmeCtx, acmeRun, actionFetch)
	if err != nil {
		t.Fatalf("acme GetApproval(own gate): %v", err)
	}

	if own.Status != store.ApprovalPending {
		t.Errorf("acme gate = %s, want still pending after the cross-tenant decide attempt", own.Status)
	}
}

// testApprovalHeldSurvivesReconcile proves a parked run is untouched by
// ReconcileInterrupted: its held status, its waiting action row, and its pending
// gate all survive a restart sweep, because a held run never looks running
// (.agents/spec/parity/workflow-language.md, "Human approval").
func testApprovalHeldSurvivesReconcile(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC)

	runID := store.NewID()
	createRun(t, db, runID, workflowAlpha, store.RunHeld, now)

	waiting := &store.ActionResult{
		RunID:      runID,
		ActionName: actionFetch,
		Runner:     runnerMock,
		Status:     store.ActionWaiting,
	}
	if err := db.UpsertActionResult(ctx, waiting); err != nil {
		t.Fatalf("stage waiting action: %v", err)
	}

	seedApproval(t, db, ctx, runID, actionFetch)

	if _, err := db.ReconcileInterrupted(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	run, err := db.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get held run: %v", err)
	}

	if run.Status != store.RunHeld || !run.FinishedAt.IsZero() {
		t.Errorf("held run = %s finished=%v, want held and unfinished after reconcile", run.Status, run.FinishedAt)
	}

	results, err := db.ListActionResults(ctx, runID)
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}

	if len(results) != 1 || results[0].Status != store.ActionWaiting {
		t.Errorf("action rows = %#v, want one still waiting", results)
	}

	gate, err := db.GetApproval(ctx, runID, actionFetch)
	if err != nil {
		t.Fatalf("get gate: %v", err)
	}

	if gate.Status != store.ApprovalPending {
		t.Errorf("gate = %s, want still pending after reconcile", gate.Status)
	}
}

// seedApproval stages one pending gate on a run in the workspace ctx carries,
// so the isolation and listing cases have a gate to find in the right tenant.
//
//nolint:revive // t leads every helper here for the t.Fatalf calls; ctx follows to carry the workspace
func seedApproval(t *testing.T, db store.Store, ctx context.Context, runID, action string) {
	t.Helper()

	err := db.CreateApproval(ctx, &store.Approval{
		RunID:       runID,
		ActionName:  action,
		WorkspaceID: store.WorkspaceFromContext(ctx),
		Message:     approvalMessage,
	})
	if err != nil {
		t.Fatalf("seed approval %s/%s: %v", runID, action, err)
	}
}

// approvalActionNames extracts action names for a failure message.
func approvalActionNames(approvals []*store.Approval) []string {
	names := make([]string, len(approvals))
	for i, approval := range approvals {
		names[i] = approval.ActionName
	}

	return names
}
