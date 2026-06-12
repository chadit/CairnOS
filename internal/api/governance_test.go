package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/pkg/runner"
)

// budgetedTOML is a workflow carrying a month budget so the budget-state RPC
// has a limit to report spend against.
const budgetedTOML = `
[workflow]
name = "budgeted"

[workflow.budget]
period = "month"
limit_usd = 50.0
on_exceed = "pause"

[action.work]
runner = "mock"
params = { output = "done", cost_usd = 1.0 }
`

const (
	budgetedName = "budgeted"
	govActor     = "cli"
	runnerMock   = "mock"
)

// seedBudgetSpend stages a finished run of the budgeted workflow carrying cost
// in the current month so SpendSince returns a non-zero figure.
func seedBudgetSpend(t *testing.T, fix *fixture, cost float64) {
	t.Helper()

	ctx := t.Context()
	runID := sqlite.NewID()

	err := fix.db.CreateRun(ctx, &store.Run{
		ID:           runID,
		WorkflowName: budgetedName,
		Status:       store.RunSucceeded,
		Trigger:      govActor,
		CreatedAt:    time.Now(),
	})
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}

	err = fix.db.UpsertActionResult(ctx, &store.ActionResult{
		RunID:      runID,
		ActionName: "work",
		Runner:     runnerMock,
		Status:     store.ActionSucceeded,
		Usage:      runner.Usage{CostUSD: cost},
	})
	if err != nil {
		t.Fatalf("seed action: %v", err)
	}
}

func TestGovernancePauseResumeRoundTrip(t *testing.T) {
	t.Parallel()

	fix := startServer(t)
	ctx := t.Context()

	pauseResp, err := fix.governance.PauseWorkflow(ctx, &cairnosv1.PauseWorkflowRequest{
		WorkflowName: budgetedName,
		Reason:       "manual hold",
	})
	if err != nil {
		t.Fatalf("pause: %v", err)
	}

	if !pauseResp.GetPaused() {
		t.Fatal("pause response did not report paused")
	}

	_, paused, err := fix.db.GetWorkflowPause(ctx, budgetedName)
	if err != nil || !paused {
		t.Fatalf("store pause = %v err = %v, want paused", paused, err)
	}

	resumeResp, err := fix.governance.ResumeWorkflow(ctx, &cairnosv1.ResumeWorkflowRequest{WorkflowName: budgetedName})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}

	if !resumeResp.GetResumed() {
		t.Fatal("resume response did not report resumed")
	}

	if _, paused, _ := fix.db.GetWorkflowPause(ctx, budgetedName); paused {
		t.Fatal("workflow still paused after resume")
	}

	// Both control actions wrote audit rows.
	audit, err := fix.governance.ListAudit(ctx, &cairnosv1.ListAuditRequest{})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}

	if len(audit.GetEntries()) < 2 {
		t.Fatalf("audit rows = %d, want at least 2 (pause and resume)", len(audit.GetEntries()))
	}

	// Newest-first: the resume row leads.
	if audit.GetEntries()[0].GetAction() != store.AuditActionResume {
		t.Errorf("newest audit action = %q, want resume", audit.GetEntries()[0].GetAction())
	}
}

func TestGovernanceBudgetStateReportsSpend(t *testing.T) {
	t.Parallel()

	fix := startServer(t)
	ctx := t.Context()

	seedBudgetSpend(t, fix, 12.5)

	state, err := fix.governance.GetWorkflowBudgetState(ctx, &cairnosv1.GetWorkflowBudgetStateRequest{WorkflowName: budgetedName})
	if err != nil {
		t.Fatalf("budget state: %v", err)
	}

	if !state.GetHasBudget() {
		t.Fatal("budget state reported no budget for a budgeted workflow")
	}

	if state.GetPeriod() != "month" || state.GetLimitUsd() != 50.0 || state.GetOnExceed() != "pause" {
		t.Errorf("budget state = period %q limit %v on_exceed %q, want month/50/pause",
			state.GetPeriod(), state.GetLimitUsd(), state.GetOnExceed())
	}

	if state.GetSpendUsd() != 12.5 {
		t.Errorf("spend = %v, want 12.5", state.GetSpendUsd())
	}

	if state.GetPaused() {
		t.Error("budget state reported paused for an unpaused workflow")
	}
}

func TestGovernanceBudgetStateNoBudget(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	// hello.toml declares no budget.
	state, err := fix.governance.GetWorkflowBudgetState(t.Context(), &cairnosv1.GetWorkflowBudgetStateRequest{WorkflowName: "hello"})
	if err != nil {
		t.Fatalf("budget state: %v", err)
	}

	if state.GetHasBudget() {
		t.Error("budget state reported a budget for a budgetless workflow")
	}
}

func TestGovernanceTerminateUnknownRun(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	_, err := fix.governance.TerminateRun(t.Context(), &cairnosv1.TerminateRunRequest{RunId: "no-such-run"})
	if err == nil {
		t.Fatal("terminate of unknown run succeeded, want FAILED_PRECONDITION")
	}

	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("terminate error code = %s, want FailedPrecondition", status.Code(err))
	}
}

func TestGovernanceListAuditOverGateway(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	// Pause over gRPC to write an audit row, then read the audit log over the
	// HTTP gateway to prove both transports reach the same service.
	if _, err := fix.governance.PauseWorkflow(t.Context(), &cairnosv1.PauseWorkflowRequest{WorkflowName: budgetedName}); err != nil {
		t.Fatalf("pause: %v", err)
	}

	code, body := httpGet(t, fix.httpBase+"/v1/governance/audit")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/governance/audit = %d, body %s", code, body)
	}

	var resp struct {
		Entries []struct {
			Action string `json:"action"`
			Target string `json:"target"`
		} `json:"entries"`
	}

	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode audit body: %v", err)
	}

	if len(resp.Entries) == 0 {
		t.Fatal("gateway audit returned no entries after a pause")
	}

	if resp.Entries[0].Action != store.AuditActionPause || resp.Entries[0].Target != budgetedName {
		t.Errorf("gateway audit newest = %+v, want a pause of %s", resp.Entries[0], budgetedName)
	}
}
