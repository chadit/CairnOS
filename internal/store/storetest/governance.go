package storetest

import (
	"math"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Governance fixture values; goconst applies to this package's tests.
const (
	actorAdmin    = "admin-1"
	auditDetail   = "monthly budget exceeded"
	pauseReason   = "manual hold"
	costPerAction = 0.25
	auditFirst    = "audit-1"
	auditSecond   = "audit-2"
	auditThird    = "audit-3"
	auditOld      = "audit-old"
	reasonRepause = "again"
	// floatEpsilon bounds the spend-sum comparison: cost rides through JSON
	// and a SQL SUM, so an exact float match is the wrong assertion.
	floatEpsilon = 1e-9
)

// runnerUsageWithCost builds a usage value carrying only an estimated cost,
// which is all SpendSince reads.
func runnerUsageWithCost(cost float64) runner.Usage {
	return runner.Usage{CostUSD: cost}
}

// floatNear reports whether two floats match within floatEpsilon.
func floatNear(got, want float64) bool {
	return math.Abs(got-want) < floatEpsilon
}

// stageActionCost stages one run of workflow with a single action carrying
// costPerAction in usage, so SpendSince has spend to sum.
func stageActionCost(t *testing.T, db store.Store, runID, workflow string, created time.Time) {
	t.Helper()

	createRun(t, db, runID, workflow, store.RunSucceeded, created)

	err := db.UpsertActionResult(t.Context(), &store.ActionResult{
		RunID:      runID,
		ActionName: actionFetch,
		Runner:     runnerMock,
		Status:     store.ActionSucceeded,
		Usage:      runnerUsageWithCost(costPerAction),
	})
	if err != nil {
		t.Fatalf("stage action cost for %s: %v", runID, err)
	}
}

func testAuditAppendListNewestFirst[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	entries := []*store.AuditEntry{
		{ID: auditFirst, Actor: actorAdmin, Action: store.AuditActionPause, Target: workflowAlpha, Detail: auditDetail, CreatedAt: base},
		{ID: auditSecond, Actor: actorAdmin, Action: store.AuditActionResume, Target: workflowAlpha, CreatedAt: base.Add(time.Hour)},
		{ID: auditThird, Actor: triggerCLI, Action: store.AuditActionTerminate, Target: runFirst, CreatedAt: base.Add(twoUnits * time.Hour)},
	}
	for _, entry := range entries {
		if err := db.AppendAudit(ctx, entry); err != nil {
			t.Fatalf("append audit %s: %v", entry.ID, err)
		}
	}

	listed, err := db.ListAudit(ctx, store.AuditFilter{})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}

	wantOrder := []string{auditThird, auditSecond, auditFirst}
	if len(listed) != len(wantOrder) {
		t.Fatalf("listed %d audit rows, want %d", len(listed), len(wantOrder))
	}

	for idx, want := range wantOrder {
		if listed[idx].ID != want {
			t.Errorf("audit[%d] id = %q, want %q", idx, listed[idx].ID, want)
		}
	}

	first := listed[twoUnits]
	if first.Actor != actorAdmin || first.Action != store.AuditActionPause || first.Target != workflowAlpha || first.Detail != auditDetail {
		t.Errorf("oldest audit = %+v, want the staged pause row", first)
	}

	if !first.CreatedAt.Equal(base) {
		t.Errorf("audit created_at = %v, want %v", first.CreatedAt, base)
	}

	limited, err := db.ListAudit(ctx, store.AuditFilter{Limit: 1})
	if err != nil {
		t.Fatalf("list audit with limit: %v", err)
	}

	if len(limited) != 1 || limited[0].ID != auditThird {
		t.Errorf("limited audit = %+v, want only the newest row", limited)
	}
}

func testAuditDefaultsStampIDAndTime[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	before := time.Now().Add(-time.Minute)

	if err := db.AppendAudit(ctx, &store.AuditEntry{Actor: "local", Action: store.AuditActionPause, Target: workflowBeta}); err != nil {
		t.Fatalf("append audit: %v", err)
	}

	listed, err := db.ListAudit(ctx, store.AuditFilter{})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}

	if len(listed) != 1 {
		t.Fatalf("listed %d audit rows, want 1", len(listed))
	}

	got := listed[0]
	if got.ID == "" {
		t.Error("append did not stamp an id for a zero-id entry")
	}

	if got.CreatedAt.Before(before) {
		t.Errorf("append did not stamp created_at: %v", got.CreatedAt)
	}
}

func testWorkflowPauseSetGetClear[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	if _, paused, err := db.GetWorkflowPause(ctx, workflowAlpha); err != nil || paused {
		t.Fatalf("fresh workflow paused = %v err = %v, want unpaused", paused, err)
	}

	pausedAt := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	if err := db.SetWorkflowPause(ctx, &store.WorkflowPause{
		WorkflowName: workflowAlpha,
		Actor:        actorAdmin,
		Reason:       pauseReason,
		PausedAt:     pausedAt,
	}); err != nil {
		t.Fatalf("set pause: %v", err)
	}

	pause, paused, err := db.GetWorkflowPause(ctx, workflowAlpha)
	if err != nil {
		t.Fatalf("get pause: %v", err)
	}

	if !paused {
		t.Fatal("workflow not reported paused after SetWorkflowPause")
	}

	if pause.Actor != actorAdmin || pause.Reason != pauseReason || !pause.PausedAt.Equal(pausedAt) {
		t.Errorf("pause = %+v, want the staged values", pause)
	}

	// Re-pausing overwrites rather than failing.
	if setWorkflowPauseErr := db.SetWorkflowPause(ctx, &store.WorkflowPause{WorkflowName: workflowAlpha, Actor: triggerCLI, Reason: reasonRepause}); setWorkflowPauseErr != nil {
		t.Fatalf("re-pause: %v", setWorkflowPauseErr)
	}

	pause, _, err = db.GetWorkflowPause(ctx, workflowAlpha)
	if err != nil {
		t.Fatalf("get re-paused: %v", err)
	}

	if pause.Actor != triggerCLI || pause.Reason != reasonRepause {
		t.Errorf("re-pause did not overwrite: %+v", pause)
	}

	cleared, err := db.ClearWorkflowPause(ctx, workflowAlpha)
	if err != nil {
		t.Fatalf("clear pause: %v", err)
	}

	if !cleared {
		t.Error("ClearWorkflowPause reported nothing cleared for a paused workflow")
	}

	if _, paused, getWorkflowPauseErr := db.GetWorkflowPause(ctx, workflowAlpha); getWorkflowPauseErr != nil || paused {
		t.Fatalf("workflow still paused after clear: paused = %v err = %v", paused, getWorkflowPauseErr)
	}

	// Clearing an unpaused workflow is a reported no-op, not an error.
	cleared, err = db.ClearWorkflowPause(ctx, workflowAlpha)
	if err != nil || cleared {
		t.Errorf("clearing unpaused workflow = cleared %v err %v, want false nil", cleared, err)
	}
}

func testWorkflowPauseListNewestFirst[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	base := time.Date(2026, 6, 3, 8, 0, 0, 0, time.UTC)

	if err := db.SetWorkflowPause(ctx, &store.WorkflowPause{WorkflowName: workflowAlpha, Actor: actorAdmin, PausedAt: base}); err != nil {
		t.Fatalf("pause alpha: %v", err)
	}

	if err := db.SetWorkflowPause(ctx, &store.WorkflowPause{WorkflowName: workflowBeta, Actor: actorAdmin, PausedAt: base.Add(time.Hour)}); err != nil {
		t.Fatalf("pause beta: %v", err)
	}

	pauses, err := db.ListWorkflowPauses(ctx)
	if err != nil {
		t.Fatalf("list pauses: %v", err)
	}

	if len(pauses) != twoUnits {
		t.Fatalf("listed %d pauses, want 2", len(pauses))
	}

	if pauses[0].WorkflowName != workflowBeta || pauses[1].WorkflowName != workflowAlpha {
		t.Errorf("pause order = %q, %q, want beta then alpha (newest first)", pauses[0].WorkflowName, pauses[1].WorkflowName)
	}
}

func testSpendSinceSumsCostInWindow[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// Two in-window runs of alpha, one before the window, and one of a
	// different workflow that must not leak into the sum.
	stageActionCost(t, db, "spend-in-1", workflowAlpha, periodStart.Add(time.Hour))
	stageActionCost(t, db, "spend-in-2", workflowAlpha, periodStart.Add(twoUnits*time.Hour))
	stageActionCost(t, db, "spend-before", workflowAlpha, periodStart.Add(-day))
	stageActionCost(t, db, "spend-other", workflowBeta, periodStart.Add(time.Hour))

	spend, err := db.SpendSince(ctx, workflowAlpha, periodStart)
	if err != nil {
		t.Fatalf("spend since: %v", err)
	}

	wantSpend := twoUnits * costPerAction
	if !floatNear(spend, wantSpend) {
		t.Errorf("spend = %v, want %v (only the two in-window alpha runs)", spend, wantSpend)
	}

	// A workflow with no runs at all sums to zero, not an error.
	empty, err := db.SpendSince(ctx, "no-such-workflow", periodStart)
	if err != nil {
		t.Fatalf("spend since for empty workflow: %v", err)
	}

	if empty != 0 {
		t.Errorf("empty-workflow spend = %v, want 0", empty)
	}
}

// testWorkflowPauseIsolatesByWorkspace proves two tenants can pause the same
// workflow name independently: a pause in workspace A does not pause it in
// workspace B, and resuming in one tenant leaves the other's pause standing
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy").
func testWorkflowPauseIsolatesByWorkspace[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)

	acmeCtx := store.ContextWithWorkspace(t.Context(), workspaceAcme)
	globexCtx := store.ContextWithWorkspace(t.Context(), workspaceGlobex)

	if err := db.SetWorkflowPause(acmeCtx, &store.WorkflowPause{WorkflowName: workflowAlpha, Actor: actorAdmin, Reason: pauseReason}); err != nil {
		t.Fatalf("acme pause: %v", err)
	}

	// The same workflow name is unpaused in globex: the pause did not cross the
	// tenant boundary.
	if _, paused, err := db.GetWorkflowPause(globexCtx, workflowAlpha); err != nil || paused {
		t.Fatalf("globex GetWorkflowPause = paused %v err %v, want unpaused; acme's pause leaked", paused, err)
	}

	// Globex pauses the same name independently.
	if err := db.SetWorkflowPause(globexCtx, &store.WorkflowPause{WorkflowName: workflowAlpha, Actor: triggerCLI, Reason: "globex hold"}); err != nil {
		t.Fatalf("globex pause: %v", err)
	}

	// Each tenant sees only its own pause in the listing.
	acmePauses, err := db.ListWorkflowPauses(acmeCtx)
	if err != nil {
		t.Fatalf("list acme pauses: %v", err)
	}

	if len(acmePauses) != 1 || acmePauses[0].Reason != pauseReason {
		t.Fatalf("acme pauses = %+v, want only acme's hold", acmePauses)
	}

	// Resuming in acme leaves globex's pause of the same name standing.
	cleared, err := db.ClearWorkflowPause(acmeCtx, workflowAlpha)
	if err != nil || !cleared {
		t.Fatalf("acme clear = cleared %v err %v, want cleared", cleared, err)
	}

	if _, paused, err := db.GetWorkflowPause(globexCtx, workflowAlpha); err != nil || !paused {
		t.Fatalf("globex GetWorkflowPause after acme resume = paused %v err %v, want still paused", paused, err)
	}
}

// testAuditIsolatesByWorkspace proves the audit trail is per tenant: a row
// written under one workspace is invisible to another's ListAudit
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy").
func testAuditIsolatesByWorkspace[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	acmeCtx := store.ContextWithWorkspace(t.Context(), workspaceAcme)
	globexCtx := store.ContextWithWorkspace(t.Context(), workspaceGlobex)

	if err := db.AppendAudit(acmeCtx, &store.AuditEntry{Actor: actorAdmin, Action: store.AuditActionPause, Target: workflowAlpha, CreatedAt: base}); err != nil {
		t.Fatalf("acme append audit: %v", err)
	}

	if err := db.AppendAudit(globexCtx, &store.AuditEntry{Actor: triggerCLI, Action: store.AuditActionResume, Target: workflowBeta, CreatedAt: base.Add(time.Hour)}); err != nil {
		t.Fatalf("globex append audit: %v", err)
	}

	acmeAudit, err := db.ListAudit(acmeCtx, store.AuditFilter{})
	if err != nil {
		t.Fatalf("list acme audit: %v", err)
	}

	if len(acmeAudit) != 1 || acmeAudit[0].Target != workflowAlpha {
		t.Fatalf("acme audit = %+v, want only acme's pause row; globex's row leaked", acmeAudit)
	}

	globexAudit, err := db.ListAudit(globexCtx, store.AuditFilter{})
	if err != nil {
		t.Fatalf("list globex audit: %v", err)
	}

	if len(globexAudit) != 1 || globexAudit[0].Target != workflowBeta {
		t.Fatalf("globex audit = %+v, want only globex's resume row", globexAudit)
	}
}

func testPruneLeavesAuditAndPauseRows[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	now := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)

	// An audit row and a pause flag both older than the run-retention window.
	if err := db.AppendAudit(ctx, &store.AuditEntry{ID: auditOld, Actor: actorAdmin, Action: store.AuditActionPause, Target: workflowAlpha, CreatedAt: now.Add(-100 * day)}); err != nil {
		t.Fatalf("append old audit: %v", err)
	}

	if err := db.SetWorkflowPause(ctx, &store.WorkflowPause{WorkflowName: workflowAlpha, Actor: actorAdmin, PausedAt: now.Add(-100 * day)}); err != nil {
		t.Fatalf("set old pause: %v", err)
	}

	if _, err := db.Prune(ctx, store.PruneOptions{RunsOlderThan: retentionWindow, Now: now}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	listed, err := db.ListAudit(ctx, store.AuditFilter{})
	if err != nil {
		t.Fatalf("list audit after prune: %v", err)
	}

	if len(listed) != 1 || listed[0].ID != auditOld {
		t.Errorf("audit after prune = %+v, want the old row kept (audit survives retention)", listed)
	}

	if _, paused, err := db.GetWorkflowPause(ctx, workflowAlpha); err != nil || !paused {
		t.Errorf("pause flag after prune: paused = %v err = %v, want still paused", paused, err)
	}
}
