// Package storetest is the shared behavioral spec for store.Store
// implementations. RunConformance is one suite that every backend runs
// against a fresh store: the sqlite backend calls it, and the postgres
// backend calls it when a test DSN is configured. One spec, two backends,
// so a behavioral difference between them fails a test instead of going
// unnoticed (.agents/spec/05-memory-and-knowledge.md, the interface-seam
// acceptance criterion carried into Phase 4c).
package storetest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Shared fixture values; goconst applies to tests in this repo's lint policy.
const (
	workflowAlpha   = "alpha-workflow"
	workflowBeta    = "beta-workflow"
	hashFixture     = "hash-1"
	triggerCLI      = "cli"
	runnerMock      = "mock"
	codeInterrupted = "engine.interrupted"
	actionFetch     = "fetch"
	actionBrain     = "brain"
	echoValue       = "hi"
	paramOutput     = "output"
	runFirst        = "run-1"
	runSecond       = "run-2"
	runThird        = "run-3"
	runLive         = "run-live"
	runOld          = "run-old"
	runMid          = "run-mid"
	runNew          = "run-new"

	suggestionTarget    = "alpha-workflow.fetch"
	suggestionRationale = "the action repeated itself"

	// Workspace fixtures for the multi-tenancy isolation cases. workspaceAcme
	// and workspaceGlobex are two separate tenants; one must never see the
	// other's runs, actions, or budget spend.
	workspaceAcme   = "acme"
	workspaceGlobex = "globex"
)

// Fixture numbers named so the suite reads as code rather than tripping the
// magic-number check (this is library code, not a _test.go file).
const (
	inputCount     = 3
	scoreValue     = 0.9
	twoUnits       = 2
	stepCount      = 3
	proposedTokens = 128
	twoDeleted     = 2
	// microFixtureNS is 123456 microseconds expressed in nanoseconds, the
	// finest precision Postgres timestamptz keeps, so the round-trip
	// assertion holds on both backends.
	microFixtureNS = 123456000

	// acmeCost and globexCost are the per-workspace action costs the budget
	// isolation case sums; the two differ so a leak across workspaces shows up
	// as the wrong total.
	acmeCost   = 1.0
	globexCost = 2.0
)

// OpenFunc hands the suite a fresh, empty store. The implementation
// registers cleanup (t.Cleanup with Close) so each subtest gets an isolated
// database. Returning the interface keeps the suite backend-agnostic.
type OpenFunc func(t *testing.T) store.Store

// newID returns a ULID, the id shape both backends use, so the suite stays
// independent of either backend's NewID.
func newID() string {
	return ulid.Make().String()
}

// RunConformance runs the full store.Store behavioral suite against the
// store openFn hands out. Each named subtest opens its own store, so they
// are independent and the caller may run the suite under t.Parallel.
func RunConformance(t *testing.T, openFn OpenFunc) {
	t.Helper()

	cases := []struct {
		name string
		run  func(t *testing.T, openFn OpenFunc)
	}{
		{"SatisfiesInterface", testSatisfiesInterface},
		{"RunActionRoundTrip", testRunActionRoundTrip},
		{"ParentRunIDRoundTrip", testParentRunIDRoundTrip},
		{"UpdateRunStatusStampsTimestamps", testUpdateRunStatusStampsTimestamps},
		{"RunNotFound", testRunNotFound},
		{"ListRunsFiltersAndOrdersNewestFirst", testListRunsFiltersAndOrdersNewestFirst},
		{"WorkspaceIsolatesRunsAndActions", testWorkspaceIsolatesRunsAndActions},
		{"AgentStepsIsolateByWorkspace", testAgentStepsIsolateByWorkspace},
		{"SpendSinceIsolatesByWorkspace", testSpendSinceIsolatesByWorkspace},
		{"ReconcileInterruptedFailsRunningWork", testReconcileInterruptedFailsRunningWork},
		{"PruneDeletesOldRunsAndTruncatesOldOutputs", testPruneRunsAndOutputs},
		{"AgentStepsRoundTripInOrder", testAgentStepsRoundTripInOrder},
		{"ConcurrentUpsertActionResult", testConcurrentUpsertActionResult},
		{"SuggestionRoundTrip", testSuggestionRoundTrip},
		{"SaveSuggestionDefaults", testSaveSuggestionDefaults},
		{"ListSuggestionsFilters", testListSuggestionsFilters},
		{"UpdateSuggestionStatus", testUpdateSuggestionStatus},
		{"SuggestionNotFound", testSuggestionNotFound},
		{"SuggestionIsolatesByWorkspace", testSuggestionIsolatesByWorkspace},
		{"PruneDeletesStaleSuggestions", testPruneDeletesStaleSuggestions},
		{"PruneWithoutRunWindowKeepsSuggestions", testPruneKeepsSuggestionsWithoutRunWindow},
		{"AuditAppendListNewestFirst", testAuditAppendListNewestFirst},
		{"AuditDefaultsStampIDAndTime", testAuditDefaultsStampIDAndTime},
		{"WorkflowPauseSetGetClear", testWorkflowPauseSetGetClear},
		{"WorkflowPauseListNewestFirst", testWorkflowPauseListNewestFirst},
		{"WorkflowPauseIsolatesByWorkspace", testWorkflowPauseIsolatesByWorkspace},
		{"AuditIsolatesByWorkspace", testAuditIsolatesByWorkspace},
		{"SpendSinceSumsCostInWindow", testSpendSinceSumsCostInWindow},
		{"PruneLeavesAuditAndPauseRows", testPruneLeavesAuditAndPauseRows},
		{"WorkflowVersionRoundTrip", testWorkflowVersionRoundTrip},
		{"WorkflowVersionSaveStampsTime", testWorkflowVersionSaveStampsTime},
		{"WorkflowVersionReSaveKeepsOriginal", testWorkflowVersionReSaveKeepsOriginal},
		{"WorkflowVersionNotFound", testWorkflowVersionNotFound},
		{"WorkflowVersionListNewestFirst", testWorkflowVersionListNewestFirst},
		{"PruneLeavesWorkflowVersions", testPruneLeavesWorkflowVersions},
		{"ChatSessionRoundTrip", testChatSessionRoundTrip},
		{"ChatSessionSeedsSystemRole", testChatSessionSeedsSystemRole},
		{"ChatSessionNotFound", testChatSessionNotFound},
		{"ChatSessionListNewestFirst", testChatSessionListNewestFirst},
		{"ChatSessionIsolatesByWorkspace", testChatSessionIsolatesByWorkspace},
		{"ChatSearchRanksBestMatchFirst", testChatSearchRanksBestMatchFirst},
		{"ChatSearchFiltersBySession", testChatSearchFiltersBySession},
		{"ChatSearchLimitsResults", testChatSearchLimitsResults},
		{"ChatSearchIsolatesByWorkspace", testChatSearchIsolatesByWorkspace},
		{"ChatSearchTreatsQuerySyntaxLiterally", testChatSearchTreatsQuerySyntaxLiterally},
		{"ChatSearchEmptyTextReturnsNothing", testChatSearchEmptyTextReturnsNothing},
		{"ApprovalRoundTrip", testApprovalRoundTrip},
		{"ApprovalNotFound", testApprovalNotFound},
		{"ApprovalDecideOnce", testApprovalDecideOnce},
		{"ApprovalListPending", testApprovalListPending},
		{"ApprovalIsolatesByWorkspace", testApprovalIsolatesByWorkspace},
		{"ApprovalHeldSurvivesReconcile", testApprovalHeldSurvivesReconcile},
		{"BridgeSessionRoundTrip", testBridgeSessionRoundTrip},
		{"BridgeSessionRebindUpserts", testBridgeSessionRebindUpserts},
		{"BridgeSessionNotFound", testBridgeSessionNotFound},
		{"BridgeSessionIsolatesByWorkspace", testBridgeSessionIsolatesByWorkspace},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t, openFn)
		})
	}
}

// createRun stages a run row directly in the given status, bypassing the
// lifecycle transitions tests that exercise UpdateRunStatus separately.
func createRun(t *testing.T, db store.Store, id, workflow string, status store.RunStatus, created time.Time) {
	t.Helper()

	err := db.CreateRun(t.Context(), &store.Run{
		ID:           id,
		WorkflowName: workflow,
		WorkflowHash: hashFixture,
		Status:       status,
		Trigger:      triggerCLI,
		CreatedAt:    created,
	})
	if err != nil {
		t.Fatalf("create run %s: %v", id, err)
	}
}

// seedSuggestion stages one suggestion row with the given id, status, and
// creation time.
func seedSuggestion(t *testing.T, db store.Store, id string, status store.SuggestionStatus, created time.Time) {
	t.Helper()

	err := db.SaveSuggestion(t.Context(), &store.Suggestion{
		ID:        id,
		Kind:      store.SuggestionCaching,
		Target:    suggestionTarget,
		Rationale: suggestionRationale,
		Status:    status,
		CreatedAt: created,
	})
	if err != nil {
		t.Fatalf("save suggestion %s: %v", id, err)
	}
}

func testSatisfiesInterface(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)

	if _, err := db.ListRuns(t.Context(), store.ListFilter{}); err != nil {
		t.Fatalf("list runs through interface: %v", err)
	}
}

func testRunActionRoundTrip(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	// Microsecond precision is the contract both backends honor: Postgres
	// timestamptz is microsecond-resolution, so the suite never asserts
	// sub-microsecond fidelity that sqlite would keep but Postgres would
	// truncate. Real timestamps come from time.Now and round-trip fine.
	created := time.Date(2026, 6, 1, 12, 30, 15, microFixtureNS, time.UTC)
	runID := newID()
	inputs := map[string]any{"ticker": "GME", "count": float64(inputCount)}

	err := db.CreateRun(ctx, &store.Run{
		ID:           runID,
		WorkflowName: workflowAlpha,
		WorkflowHash: hashFixture,
		Status:       store.RunPending,
		Inputs:       inputs,
		Trigger:      triggerCLI,
		CreatedAt:    created,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	want := &store.ActionResult{
		RunID:      runID,
		ActionName: actionFetch,
		Runner:     runnerMock,
		Status:     store.ActionFailed,
		Attempt:    2,
		Output:     map[string]any{"score": float64(scoreValue), "note": "partial"},
		Usage: runner.Usage{
			InputTokens:  100,
			OutputTokens: 50,
			CachedTokens: 10,
			CostUSD:      0.25,
			Latency:      1500 * time.Millisecond,
			Retries:      1,
		},
		Meta:       map[string]string{"model": "mock-1", "request_id": "req-9"},
		Error:      cairnerr.New("runner.flaky", "second attempt failed").WithDetail("attempt", "2"),
		StartedAt:  created.Add(time.Second),
		FinishedAt: created.Add(twoUnits * time.Second),
	}

	if err := db.UpsertActionResult(ctx, want); err != nil {
		t.Fatalf("upsert action result: %v", err)
	}

	run, err := db.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}

	if run.WorkflowName != workflowAlpha || run.WorkflowHash != hashFixture || run.Trigger != triggerCLI {
		t.Errorf("run identity = %q/%q/%q, want staged values", run.WorkflowName, run.WorkflowHash, run.Trigger)
	}

	if run.WorkspaceID != "default" {
		t.Errorf("workspace = %q, want default", run.WorkspaceID)
	}

	if !run.CreatedAt.Equal(created) {
		t.Errorf("created_at = %v, want %v", run.CreatedAt, created)
	}

	if !reflect.DeepEqual(run.Inputs, inputs) {
		t.Errorf("inputs = %#v, want %#v", run.Inputs, inputs)
	}

	results, err := db.ListActionResults(ctx, runID)
	if err != nil {
		t.Fatalf("list action results: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("got %d action results, want 1", len(results))
	}

	got := results[0]
	if !got.StartedAt.Equal(want.StartedAt) || !got.FinishedAt.Equal(want.FinishedAt) {
		t.Errorf("timestamps = %v/%v, want %v/%v", got.StartedAt, got.FinishedAt, want.StartedAt, want.FinishedAt)
	}

	// Zero the times already compared with Equal; DeepEqual would trip on
	// time.Time internals (monotonic clock, location pointers) that differ
	// between backends.
	got.StartedAt, got.FinishedAt = time.Time{}, time.Time{}
	want.StartedAt, want.FinishedAt = time.Time{}, time.Time{}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("action result = %#v, want %#v", got, want)
	}
}

// testParentRunIDRoundTrip proves runs.parent_run_id survives the store: a
// child run created with ParentRunID set reads it back, while a root run reads
// the empty string. Replay, workflow-call, and error-workflow children all
// rely on this link (.agents/spec/parity/workflow-language.md).
func testParentRunIDRoundTrip(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	parentID := newID()
	childID := newID()
	created := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	createRun(t, db, parentID, workflowAlpha, store.RunFailed, created)

	err := db.CreateRun(ctx, &store.Run{
		ID:           childID,
		WorkflowName: workflowAlpha,
		WorkflowHash: hashFixture,
		Status:       store.RunPending,
		Trigger:      "replay",
		ParentRunID:  parentID,
		CreatedAt:    created.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("create child run: %v", err)
	}

	child, err := db.GetRun(ctx, childID)
	if err != nil {
		t.Fatalf("get child run: %v", err)
	}

	if child.ParentRunID != parentID {
		t.Errorf("child parent_run_id = %q, want %q", child.ParentRunID, parentID)
	}

	parent, err := db.GetRun(ctx, parentID)
	if err != nil {
		t.Fatalf("get parent run: %v", err)
	}

	if parent.ParentRunID != "" {
		t.Errorf("root run parent_run_id = %q, want empty", parent.ParentRunID)
	}
}

func testUpdateRunStatusStampsTimestamps(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	runID := newID()

	createRun(t, db, runID, workflowAlpha, store.RunPending, time.Now())

	run, err := db.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get pending run: %v", err)
	}

	if !run.StartedAt.IsZero() || !run.FinishedAt.IsZero() {
		t.Fatalf("pending run has stamps: started=%v finished=%v", run.StartedAt, run.FinishedAt)
	}

	if err := db.UpdateRunStatus(ctx, runID, store.RunRunning, nil); err != nil {
		t.Fatalf("transition to running: %v", err)
	}

	run, err = db.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get running run: %v", err)
	}

	if run.StartedAt.IsZero() {
		t.Fatal("first transition to running did not stamp started_at")
	}

	if !run.FinishedAt.IsZero() {
		t.Fatalf("running run has finished_at %v", run.FinishedAt)
	}

	firstStart := run.StartedAt

	if err := db.UpdateRunStatus(ctx, runID, store.RunRunning, nil); err != nil {
		t.Fatalf("repeat transition to running: %v", err)
	}

	run, err = db.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get re-running run: %v", err)
	}

	if !run.StartedAt.Equal(firstStart) {
		t.Errorf("second running transition moved started_at from %v to %v", firstStart, run.StartedAt)
	}

	failure := cairnerr.New("runner.boom", "exploded")
	if err := db.UpdateRunStatus(ctx, runID, store.RunFailed, failure); err != nil {
		t.Fatalf("transition to failed: %v", err)
	}

	run, err = db.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get failed run: %v", err)
	}

	if run.Status != store.RunFailed || run.FinishedAt.IsZero() {
		t.Errorf("terminal run = %s finished=%v, want failed with stamp", run.Status, run.FinishedAt)
	}

	if run.Error == nil || run.Error.Code != "runner.boom" {
		t.Errorf("run error = %#v, want code runner.boom", run.Error)
	}
}

func testRunNotFound(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	if _, err := db.GetRun(ctx, "missing"); !errors.Is(err, store.ErrRunNotFound) {
		t.Errorf("GetRun error = %v, want ErrRunNotFound", err)
	}

	err := db.UpdateRunStatus(ctx, "missing", store.RunRunning, nil)
	if !errors.Is(err, store.ErrRunNotFound) {
		t.Errorf("UpdateRunStatus error = %v, want ErrRunNotFound", err)
	}
}

func testListRunsFiltersAndOrdersNewestFirst(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	base := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	createRun(t, db, runFirst, workflowAlpha, store.RunSucceeded, base)
	createRun(t, db, runSecond, workflowAlpha, store.RunFailed, base.Add(time.Hour))
	createRun(t, db, runThird, workflowBeta, store.RunSucceeded, base.Add(twoUnits*time.Hour))

	cases := []struct {
		name   string
		filter store.ListFilter
		want   []string
	}{
		{
			name:   "no filter returns everything newest first",
			filter: store.ListFilter{},
			want:   []string{runThird, runSecond, runFirst},
		},
		{
			name:   "workflow name narrows",
			filter: store.ListFilter{WorkflowName: workflowAlpha},
			want:   []string{runSecond, runFirst},
		},
		{
			name:   "status narrows",
			filter: store.ListFilter{Status: store.RunSucceeded},
			want:   []string{runThird, runFirst},
		},
		{
			name:   "limit caps the newest",
			filter: store.ListFilter{Limit: 2},
			want:   []string{runThird, runSecond},
		},
		{
			name:   "workflow and status combine",
			filter: store.ListFilter{WorkflowName: workflowAlpha, Status: store.RunFailed},
			want:   []string{runSecond},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runs, err := db.ListRuns(t.Context(), tt.filter)
			if err != nil {
				t.Fatalf("list runs: %v", err)
			}

			ids := make([]string, len(runs))
			for i, run := range runs {
				ids[i] = run.ID
			}

			if !reflect.DeepEqual(ids, tt.want) {
				t.Errorf("run ids = %v, want %v", ids, tt.want)
			}
		})
	}
}

// testWorkspaceIsolatesRunsAndActions is the multi-tenancy invariant the spec
// rests on: a run created in one workspace is invisible to a context scoped to
// another, for GetRun, ListRuns, and ListActionResults
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy"). The context carries
// the workspace; the store filters by it.
func testWorkspaceIsolatesRunsAndActions(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	base := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	acmeCtx := store.ContextWithWorkspace(t.Context(), workspaceAcme)
	globexCtx := store.ContextWithWorkspace(t.Context(), workspaceGlobex)

	acmeRun := newID()
	globexRun := newID()

	createRunIn(t, db, acmeCtx, acmeRun, base)
	createRunIn(t, db, globexCtx, globexRun, base.Add(time.Hour))

	upsertActionIn(t, db, acmeCtx, acmeRun, actionFetch)
	upsertActionIn(t, db, globexCtx, globexRun, actionFetch)

	// Acme sees only its own run.
	acmeRuns, err := db.ListRuns(acmeCtx, store.ListFilter{})
	if err != nil {
		t.Fatalf("list acme runs: %v", err)
	}

	if len(acmeRuns) != 1 || acmeRuns[0].ID != acmeRun {
		t.Fatalf("acme runs = %v, want only %s", runIDs(acmeRuns), acmeRun)
	}

	// Acme cannot GetRun the globex run; it reads as not found across the
	// tenant boundary.
	if _, err := db.GetRun(acmeCtx, globexRun); !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("acme GetRun(globex run) error = %v, want ErrRunNotFound", err)
	}

	// Acme cannot list the globex run's actions.
	actions, err := db.ListActionResults(acmeCtx, globexRun)
	if err != nil {
		t.Fatalf("acme ListActionResults(globex run): %v", err)
	}

	if len(actions) != 0 {
		t.Fatalf("acme saw %d actions of the globex run, want 0", len(actions))
	}

	// Acme sees its own run's actions.
	ownActions, err := db.ListActionResults(acmeCtx, acmeRun)
	if err != nil {
		t.Fatalf("acme ListActionResults(own run): %v", err)
	}

	if len(ownActions) != 1 {
		t.Fatalf("acme saw %d actions of its own run, want 1", len(ownActions))
	}
}

// testSpendSinceIsolatesByWorkspace proves budgets are per-workspace: two
// workspaces running the same workflow name accumulate spend separately, so
// one tenant's runs never count against another's budget.
func testSpendSinceIsolatesByWorkspace(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	base := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	acmeCtx := store.ContextWithWorkspace(t.Context(), workspaceAcme)
	globexCtx := store.ContextWithWorkspace(t.Context(), workspaceGlobex)

	acmeRun := newID()
	globexRun := newID()

	createRunIn(t, db, acmeCtx, acmeRun, base)
	createRunIn(t, db, globexCtx, globexRun, base)

	upsertActionCost(t, db, acmeCtx, acmeRun, acmeCost)
	upsertActionCost(t, db, globexCtx, globexRun, globexCost)

	acmeSpend, err := db.SpendSince(acmeCtx, workflowAlpha, base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("acme SpendSince: %v", err)
	}

	if acmeSpend != acmeCost {
		t.Fatalf("acme spend = %v, want %v; globex spend leaked into acme's budget", acmeSpend, acmeCost)
	}

	globexSpend, err := db.SpendSince(globexCtx, workflowAlpha, base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("globex SpendSince: %v", err)
	}

	if globexSpend != globexCost {
		t.Fatalf("globex spend = %v, want %v", globexSpend, globexCost)
	}
}

// createRunIn stages a pending run of workflowAlpha in the workspace ctx
// carries, the workflow every isolation case shares. It stamps WorkspaceID
// from the context the way the engine does (engine.RunWorkflow reads
// store.WorkspaceFromContext), so the row lands in the right tenant for the
// workspace-scoped reads the isolation cases make afterward.
//
//nolint:revive // t leads every helper here for the t.Fatalf calls; ctx follows to carry the workspace
func createRunIn(t *testing.T, db store.Store, ctx context.Context, id string, created time.Time) {
	t.Helper()

	err := db.CreateRun(ctx, &store.Run{
		ID:           id,
		WorkflowName: workflowAlpha,
		WorkflowHash: hashFixture,
		WorkspaceID:  store.WorkspaceFromContext(ctx),
		Status:       store.RunPending,
		Trigger:      triggerCLI,
		CreatedAt:    created,
	})
	if err != nil {
		t.Fatalf("create run %s: %v", id, err)
	}
}

// upsertActionIn stages one succeeded action on a run in the workspace ctx
// carries, so the isolation test's workspace-scoped reads have a row to find.
//
//nolint:revive // t leads every helper here for the t.Fatalf calls; ctx follows to carry the workspace
func upsertActionIn(t *testing.T, db store.Store, ctx context.Context, runID, action string) {
	t.Helper()

	err := db.UpsertActionResult(ctx, &store.ActionResult{
		RunID:      runID,
		ActionName: action,
		Runner:     runnerMock,
		Status:     store.ActionSucceeded,
	})
	if err != nil {
		t.Fatalf("upsert action %s/%s: %v", runID, action, err)
	}
}

// upsertActionCost stages one action carrying a cost in the workspace ctx
// carries, so SpendSince has something to sum.
//
//nolint:revive // t leads every helper here for the t.Fatalf calls; ctx follows to carry the workspace
func upsertActionCost(t *testing.T, db store.Store, ctx context.Context, runID string, cost float64) {
	t.Helper()

	err := db.UpsertActionResult(ctx, &store.ActionResult{
		RunID:      runID,
		ActionName: actionFetch,
		Runner:     runnerMock,
		Status:     store.ActionSucceeded,
		Usage:      runner.Usage{CostUSD: cost},
	})
	if err != nil {
		t.Fatalf("upsert action cost for %s: %v", runID, err)
	}
}

// runIDs extracts run ids for a failure message.
func runIDs(runs []*store.Run) []string {
	ids := make([]string, len(runs))
	for i, run := range runs {
		ids[i] = run.ID
	}

	return ids
}

func testReconcileInterruptedFailsRunningWork(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	now := time.Date(2026, 5, 2, 9, 0, 0, 0, time.UTC)

	const runDone = "run-done"

	createRun(t, db, runLive, workflowAlpha, store.RunRunning, now)
	createRun(t, db, runDone, workflowAlpha, store.RunSucceeded, now)

	stage := []*store.ActionResult{
		{RunID: runLive, ActionName: actionFetch, Runner: runnerMock, Status: store.ActionRunning},
		{RunID: runLive, ActionName: "score", Runner: runnerMock, Status: store.ActionSucceeded},
		{RunID: runDone, ActionName: actionFetch, Runner: runnerMock, Status: store.ActionSucceeded},
	}
	for _, result := range stage {
		if err := db.UpsertActionResult(ctx, result); err != nil {
			t.Fatalf("stage action %s/%s: %v", result.RunID, result.ActionName, err)
		}
	}

	reconciled, err := db.ReconcileInterrupted(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if reconciled != 1 {
		t.Errorf("reconciled = %d, want 1", reconciled)
	}

	live, err := db.GetRun(ctx, runLive)
	if err != nil {
		t.Fatalf("get reconciled run: %v", err)
	}

	if live.Status != store.RunFailed || live.FinishedAt.IsZero() {
		t.Errorf("reconciled run = %s finished=%v, want failed with stamp", live.Status, live.FinishedAt)
	}

	if live.Error == nil || live.Error.Code != codeInterrupted {
		t.Errorf("reconciled run error = %#v, want code %s", live.Error, codeInterrupted)
	}

	results, err := db.ListActionResults(ctx, runLive)
	if err != nil {
		t.Fatalf("list reconciled actions: %v", err)
	}

	assertReconciledActions(t, results)

	done, err := db.GetRun(ctx, runDone)
	if err != nil {
		t.Fatalf("get untouched run: %v", err)
	}

	if done.Status != store.RunSucceeded || done.Error != nil {
		t.Errorf("untouched run = %s err=%#v, want succeeded", done.Status, done.Error)
	}
}

// assertReconciledActions expects the staged running action to be failed
// with engine.interrupted while the already finished one stays untouched.
func assertReconciledActions(t *testing.T, results []*store.ActionResult) {
	t.Helper()

	for _, result := range results {
		if result.ActionName == actionFetch {
			if result.Status != store.ActionFailed {
				t.Errorf("running action = %s, want failed", result.Status)
			}

			if result.Error == nil || result.Error.Code != codeInterrupted {
				t.Errorf("running action error = %#v, want code %s", result.Error, codeInterrupted)
			}

			continue
		}

		if result.Status != store.ActionSucceeded || result.Error != nil {
			t.Errorf("finished action = %s err=%#v, want succeeded untouched", result.Status, result.Error)
		}
	}
}

func testPruneRunsAndOutputs(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	day := 24 * time.Hour
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	createRun(t, db, runOld, workflowAlpha, store.RunSucceeded, now.Add(-100*day))
	createRun(t, db, runMid, workflowAlpha, store.RunSucceeded, now.Add(-40*day))
	createRun(t, db, runNew, workflowAlpha, store.RunSucceeded, now.Add(-day))

	for _, runID := range []string{runOld, runMid, runNew} {
		result := &store.ActionResult{
			RunID:      runID,
			ActionName: actionFetch,
			Runner:     runnerMock,
			Status:     store.ActionSucceeded,
			Output:     map[string]any{"payload": "big"},
		}
		if err := db.UpsertActionResult(ctx, result); err != nil {
			t.Fatalf("stage action for %s: %v", runID, err)
		}

		step := &store.AgentStep{
			RunID:      runID,
			ActionName: actionBrain,
			Step:       1,
			Kind:       store.StepFinal,
			Output:     "transcript payload",
			CreatedAt:  now,
		}
		if err := db.AppendAgentStep(ctx, step); err != nil {
			t.Fatalf("stage agent step for %s: %v", runID, err)
		}
	}

	stats, err := db.Prune(ctx, store.PruneOptions{
		RunsOlderThan:    90 * day,
		OutputsOlderThan: 30 * day,
		Now:              now,
	})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	// One action output plus one agent-step output truncated, both runMid's.
	if stats.RunsDeleted != 1 || stats.OutputsTruncated != 2 {
		t.Errorf("stats = %+v, want 1 run deleted and 2 outputs truncated", stats)
	}

	if _, err := db.GetRun(ctx, runOld); !errors.Is(err, store.ErrRunNotFound) {
		t.Errorf("old run lookup = %v, want ErrRunNotFound", err)
	}

	assertRunRowsDeleted(t, db, runOld)
	assertOutputsTruncated(t, db, runMid)
	assertOutputsIntact(t, db, runNew)
}

// assertRunRowsDeleted expects a pruned run to have lost its action rows
// and agent steps along with the run itself.
func assertRunRowsDeleted(t *testing.T, db store.Store, runID string) {
	t.Helper()

	results, err := db.ListActionResults(t.Context(), runID)
	if err != nil {
		t.Fatalf("list pruned actions: %v", err)
	}

	if len(results) != 0 {
		t.Errorf("pruned run kept %d action rows, want 0", len(results))
	}

	steps, err := db.ListAgentSteps(t.Context(), runID, actionBrain)
	if err != nil {
		t.Fatalf("list pruned agent steps: %v", err)
	}

	if len(steps) != 0 {
		t.Errorf("pruned run kept %d agent steps, want 0: steps delete with their run", len(steps))
	}
}

// assertOutputsTruncated expects the action and agent-step rows to survive
// with their output_json nulled.
func assertOutputsTruncated(t *testing.T, db store.Store, runID string) {
	t.Helper()

	results, err := db.ListActionResults(t.Context(), runID)
	if err != nil {
		t.Fatalf("list truncated actions: %v", err)
	}

	if len(results) != 1 || results[0].Output != nil {
		t.Errorf("truncated actions = %#v, want one row with nulled output", results)
	}

	if results[0].Status != store.ActionSucceeded {
		t.Errorf("truncation changed status to %s", results[0].Status)
	}

	steps, err := db.ListAgentSteps(t.Context(), runID, actionBrain)
	if err != nil {
		t.Fatalf("list truncated agent steps: %v", err)
	}

	if len(steps) != 1 || steps[0].Output != nil || steps[0].Kind != store.StepFinal {
		t.Errorf("truncated agent steps = %#v, want one row with nulled output and its kind kept", steps)
	}
}

// assertOutputsIntact expects a young run's outputs untouched by the sweep.
func assertOutputsIntact(t *testing.T, db store.Store, runID string) {
	t.Helper()

	results, err := db.ListActionResults(t.Context(), runID)
	if err != nil {
		t.Fatalf("list intact actions: %v", err)
	}

	if len(results) != 1 || results[0].Output == nil {
		t.Errorf("intact actions = %#v, want one row with output kept", results)
	}

	steps, err := db.ListAgentSteps(t.Context(), runID, actionBrain)
	if err != nil {
		t.Fatalf("list intact agent steps: %v", err)
	}

	if len(steps) != 1 || steps[0].Output != "transcript payload" {
		t.Errorf("intact agent steps = %#v, want the transcript output kept", steps)
	}
}

func testAgentStepsRoundTripInOrder(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	runID := newID()

	createRun(t, db, runID, workflowAlpha, store.RunRunning, time.Now())

	staged := []*store.AgentStep{
		{
			RunID:      runID,
			ActionName: actionBrain,
			Step:       1,
			Kind:       store.StepDriverCall,
			Output:     map[string]any{"tool": runnerMock, "arguments": map[string]any{paramOutput: echoValue}},
			Usage:      runner.Usage{InputTokens: 10, OutputTokens: 5, CostUSD: 0.5},
			CreatedAt:  time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			RunID:      runID,
			ActionName: actionBrain,
			Step:       2,
			Kind:       store.StepToolCall,
			Tool:       runnerMock,
			Input:      map[string]any{paramOutput: echoValue},
			Output:     echoValue,
			CreatedAt:  time.Date(2026, 6, 1, 0, 0, 1, 0, time.UTC),
		},
		{
			RunID:      runID,
			ActionName: actionBrain,
			Step:       3,
			Kind:       store.StepFinal,
			Output:     "done",
			Usage:      runner.Usage{CostUSD: 0.25},
			CreatedAt:  time.Date(2026, 6, 1, 0, 0, 2, 0, time.UTC),
		},
	}

	// Append out of order to prove listing orders by step, not insertion.
	for _, idx := range []int{2, 0, 1} {
		if err := db.AppendAgentStep(ctx, staged[idx]); err != nil {
			t.Fatalf("append step %d: %v", staged[idx].Step, err)
		}
	}

	steps, err := db.ListAgentSteps(ctx, runID, actionBrain)
	if err != nil {
		t.Fatalf("list agent steps: %v", err)
	}

	if len(steps) != stepCount {
		t.Fatalf("listed %d steps, want 3", len(steps))
	}

	for idx, step := range steps {
		want := staged[idx]

		if step.Step != want.Step || step.Kind != want.Kind || step.Tool != want.Tool {
			t.Errorf("step %d = %+v, want kind %q tool %q", want.Step, step, want.Kind, want.Tool)
		}

		if step.Usage != want.Usage {
			t.Errorf("step %d usage = %+v, want %+v", want.Step, step.Usage, want.Usage)
		}

		if step.CreatedAt.IsZero() {
			t.Errorf("step %d created_at not persisted", want.Step)
		}
	}

	// JSON round trips land as generic shapes: ints come back float64.
	wantOutput := map[string]any{"tool": runnerMock, "arguments": map[string]any{paramOutput: echoValue}}
	if !reflect.DeepEqual(steps[0].Output, wantOutput) {
		t.Errorf("driver step output = %#v, want %#v", steps[0].Output, wantOutput)
	}

	if !reflect.DeepEqual(steps[1].Input, map[string]any{paramOutput: echoValue}) || steps[1].Output != echoValue {
		t.Errorf("tool step io = in %#v out %#v, want the staged arguments and result", steps[1].Input, steps[1].Output)
	}

	other, err := db.ListAgentSteps(ctx, runID, "other_action")
	if err != nil {
		t.Fatalf("list other action steps: %v", err)
	}

	if len(other) != 0 {
		t.Errorf("other action listed %d steps, want 0", len(other))
	}
}

func testConcurrentUpsertActionResult(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	runID := newID()

	createRun(t, db, runID, workflowAlpha, store.RunRunning, time.Now())

	const (
		workers          = 8
		updatesPerWorker = 5
	)

	var wg sync.WaitGroup

	for worker := range workers {
		wg.Go(func() {
			for attempt := range updatesPerWorker {
				result := &store.ActionResult{
					RunID:      runID,
					ActionName: fmt.Sprintf("action-%d", worker),
					Runner:     runnerMock,
					Status:     store.ActionRunning,
					Attempt:    attempt,
				}
				if err := db.UpsertActionResult(ctx, result); err != nil {
					t.Errorf("worker %d attempt %d: %v", worker, attempt, err)
				}
			}
		})
	}

	wg.Wait()

	results, err := db.ListActionResults(ctx, runID)
	if err != nil {
		t.Fatalf("list action results: %v", err)
	}

	if len(results) != workers {
		t.Errorf("got %d action rows, want %d", len(results), workers)
	}

	for _, result := range results {
		if result.Attempt != updatesPerWorker-1 {
			t.Errorf("action %s attempt = %d, want %d", result.ActionName, result.Attempt, updatesPerWorker-1)
		}
	}
}

func testSuggestionRoundTrip(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	created := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	want := &store.Suggestion{
		ID:        newID(),
		Kind:      store.SuggestionParamTuning,
		Target:    suggestionTarget,
		Rationale: suggestionRationale,
		Evidence: map[string]any{
			"runner":     "anthropic",
			"max_tokens": float64(4096),
		},
		Proposed: map[string]any{
			"max_tokens": float64(proposedTokens),
		},
		EstSavings: store.Savings{
			CostUSDPerRun: 0.42,
			LatencyDelta:  250 * time.Millisecond,
			Confidence:    store.ConfidenceLow,
		},
		Status:    store.SuggestionOpen,
		CreatedAt: created,
	}

	if err := db.SaveSuggestion(ctx, want); err != nil {
		t.Fatalf("save suggestion: %v", err)
	}

	got, err := db.GetSuggestion(ctx, want.ID)
	if err != nil {
		t.Fatalf("get suggestion: %v", err)
	}

	if got.Kind != want.Kind || got.Target != want.Target || got.Rationale != want.Rationale {
		t.Fatalf("round trip mismatch: got %+v, want %+v", got, want)
	}

	if got.Status != store.SuggestionOpen {
		t.Fatalf("status = %s, want open", got.Status)
	}

	if got.EstSavings != want.EstSavings {
		t.Fatalf("savings = %+v, want %+v", got.EstSavings, want.EstSavings)
	}

	if got.Evidence["runner"] != "anthropic" || got.Evidence["max_tokens"] != float64(4096) {
		t.Fatalf("evidence = %+v, want runner and max_tokens preserved", got.Evidence)
	}

	if got.Proposed["max_tokens"] != float64(proposedTokens) {
		t.Fatalf("proposed = %+v, want max_tokens 128", got.Proposed)
	}

	if !got.CreatedAt.Equal(created) {
		t.Fatalf("created_at = %v, want %v", got.CreatedAt, created)
	}
}

func testSaveSuggestionDefaults(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	suggestion := &store.Suggestion{
		ID:        newID(),
		Kind:      store.SuggestionCaching,
		Target:    suggestionTarget,
		Rationale: suggestionRationale,
	}

	before := time.Now().Add(-time.Minute)

	if err := db.SaveSuggestion(ctx, suggestion); err != nil {
		t.Fatalf("save suggestion: %v", err)
	}

	got, err := db.GetSuggestion(ctx, suggestion.ID)
	if err != nil {
		t.Fatalf("get suggestion: %v", err)
	}

	if got.Status != store.SuggestionOpen {
		t.Fatalf("zero status persisted as %q, want open", got.Status)
	}

	if got.CreatedAt.Before(before) {
		t.Fatalf("zero CreatedAt was not stamped: %v", got.CreatedAt)
	}
}

func testListSuggestionsFilters(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	seedSuggestion(t, db, "sug-open-old", store.SuggestionOpen, base)
	seedSuggestion(t, db, "sug-open-new", store.SuggestionOpen, base.Add(time.Hour))
	seedSuggestion(t, db, "sug-approved", store.SuggestionApproved, base.Add(twoUnits*time.Hour))

	tests := []struct {
		name    string
		filter  store.SuggestionFilter
		wantIDs []string
	}{
		{
			name:    "no filter lists newest first",
			filter:  store.SuggestionFilter{},
			wantIDs: []string{"sug-approved", "sug-open-new", "sug-open-old"},
		},
		{
			name:    "status filter narrows to open",
			filter:  store.SuggestionFilter{Status: store.SuggestionOpen},
			wantIDs: []string{"sug-open-new", "sug-open-old"},
		},
		{
			name:    "limit caps the listing",
			filter:  store.SuggestionFilter{Limit: 1},
			wantIDs: []string{"sug-approved"},
		},
		{
			name:    "kind filter matches the batching rows",
			filter:  store.SuggestionFilter{Kind: store.SuggestionBatching},
			wantIDs: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			listed, err := db.ListSuggestions(ctx, tt.filter)
			if err != nil {
				t.Fatalf("list suggestions: %v", err)
			}

			gotIDs := make([]string, 0, len(listed))
			for _, suggestion := range listed {
				gotIDs = append(gotIDs, suggestion.ID)
			}

			if len(gotIDs) != len(tt.wantIDs) {
				t.Fatalf("listed %v, want %v", gotIDs, tt.wantIDs)
			}

			for idx, id := range tt.wantIDs {
				if gotIDs[idx] != id {
					t.Fatalf("listed %v, want %v", gotIDs, tt.wantIDs)
				}
			}
		})
	}
}

func testUpdateSuggestionStatus(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	seedSuggestion(t, db, "sug-review", store.SuggestionOpen, time.Now())

	if err := db.UpdateSuggestionStatus(ctx, "sug-review", store.SuggestionApproved); err != nil {
		t.Fatalf("update status: %v", err)
	}

	got, err := db.GetSuggestion(ctx, "sug-review")
	if err != nil {
		t.Fatalf("get suggestion: %v", err)
	}

	if got.Status != store.SuggestionApproved {
		t.Fatalf("status = %s, want approved", got.Status)
	}
}

func testSuggestionNotFound(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	if _, err := db.GetSuggestion(ctx, "missing"); !errors.Is(err, store.ErrSuggestionNotFound) {
		t.Fatalf("get missing suggestion err = %v, want ErrSuggestionNotFound", err)
	}

	err := db.UpdateSuggestionStatus(ctx, "missing", store.SuggestionRejected)
	if !errors.Is(err, store.ErrSuggestionNotFound) {
		t.Fatalf("update missing suggestion err = %v, want ErrSuggestionNotFound", err)
	}
}

func testPruneDeletesStaleSuggestions(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	staleAge := 90 * 24 * time.Hour

	seedSuggestion(t, db, "sug-stale", store.SuggestionOpen, now.Add(-staleAge-time.Hour))
	seedSuggestion(t, db, "sug-stale-approved", store.SuggestionApproved, now.Add(-staleAge-2*time.Hour))
	seedSuggestion(t, db, "sug-fresh", store.SuggestionOpen, now.Add(-time.Hour))

	stats, err := db.Prune(ctx, store.PruneOptions{RunsOlderThan: staleAge, Now: now})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	if stats.SuggestionsDeleted != twoDeleted {
		t.Fatalf("SuggestionsDeleted = %d, want 2", stats.SuggestionsDeleted)
	}

	if _, err := db.GetSuggestion(ctx, "sug-stale"); !errors.Is(err, store.ErrSuggestionNotFound) {
		t.Fatalf("stale suggestion survived prune: err = %v", err)
	}

	if _, err := db.GetSuggestion(ctx, "sug-fresh"); err != nil {
		t.Fatalf("fresh suggestion was pruned: %v", err)
	}
}

// testSuggestionIsolatesByWorkspace proves a suggestion saved in one workspace
// is invisible to another: GetSuggestion across the boundary returns not-found
// rather than leaking the evidence and proposed change, and ListSuggestions is
// scoped to the request tenant (.agents/spec/12-ui-and-governance.md,
// "Multi-tenancy"). A non-admin token in one tenant must not read another's
// suggestion, which embeds workflow names and costs.
func testSuggestionIsolatesByWorkspace(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	created := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	acmeCtx := store.ContextWithWorkspace(t.Context(), workspaceAcme)
	globexCtx := store.ContextWithWorkspace(t.Context(), workspaceGlobex)

	acmeID := newID()
	globexID := newID()

	saveSuggestionIn(t, db, acmeCtx, acmeID, created)
	saveSuggestionIn(t, db, globexCtx, globexID, created.Add(time.Hour))

	// Acme cannot read the globex suggestion: it is not-found across the
	// tenant boundary, so the embedded evidence never leaks.
	if _, err := db.GetSuggestion(acmeCtx, globexID); !errors.Is(err, store.ErrSuggestionNotFound) {
		t.Fatalf("acme GetSuggestion(globex id) error = %v, want ErrSuggestionNotFound", err)
	}

	// Acme reads its own suggestion.
	if _, err := db.GetSuggestion(acmeCtx, acmeID); err != nil {
		t.Fatalf("acme GetSuggestion(own id): %v", err)
	}

	// Acme's listing carries only its own suggestion.
	acmeList, err := db.ListSuggestions(acmeCtx, store.SuggestionFilter{})
	if err != nil {
		t.Fatalf("list acme suggestions: %v", err)
	}

	if len(acmeList) != 1 || acmeList[0].ID != acmeID {
		t.Fatalf("acme suggestions = %v, want only %s", suggestionIDs(acmeList), acmeID)
	}

	// A cross-tenant status flip cannot reach the globex row either: the update
	// is workspace-scoped, so acme's attempt reads as not-found.
	if err := db.UpdateSuggestionStatus(acmeCtx, globexID, store.SuggestionRejected); !errors.Is(err, store.ErrSuggestionNotFound) {
		t.Fatalf("acme UpdateSuggestionStatus(globex id) error = %v, want ErrSuggestionNotFound", err)
	}
}

// saveSuggestionIn stages one suggestion in the workspace ctx carries, so the
// isolation reads have a row to find in the right tenant.
//
//nolint:revive // t leads every helper here for the t.Fatalf calls; ctx follows to carry the workspace
func saveSuggestionIn(t *testing.T, db store.Store, ctx context.Context, id string, created time.Time) {
	t.Helper()

	err := db.SaveSuggestion(ctx, &store.Suggestion{
		ID:        id,
		Kind:      store.SuggestionCaching,
		Target:    suggestionTarget,
		Rationale: suggestionRationale,
		Status:    store.SuggestionOpen,
		CreatedAt: created,
	})
	if err != nil {
		t.Fatalf("save suggestion %s: %v", id, err)
	}
}

// suggestionIDs extracts suggestion ids for a failure message.
func suggestionIDs(suggestions []*store.Suggestion) []string {
	ids := make([]string, len(suggestions))
	for i, suggestion := range suggestions {
		ids[i] = suggestion.ID
	}

	return ids
}

// testAgentStepsIsolateByWorkspace proves a run's transcript is invisible to a
// context scoped to another workspace, matching ListActionResults: agent steps
// for a run in workspace A read as empty under a workspace-B context
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy").
func testAgentStepsIsolateByWorkspace(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	acmeCtx := store.ContextWithWorkspace(t.Context(), workspaceAcme)
	globexCtx := store.ContextWithWorkspace(t.Context(), workspaceGlobex)

	acmeRun := newID()
	createRunIn(t, db, acmeCtx, acmeRun, base)

	err := db.AppendAgentStep(acmeCtx, &store.AgentStep{
		RunID:      acmeRun,
		ActionName: actionBrain,
		Step:       1,
		Kind:       store.StepFinal,
		Output:     "acme transcript",
		CreatedAt:  base,
	})
	if err != nil {
		t.Fatalf("append acme agent step: %v", err)
	}

	// Acme reads its own transcript.
	ownSteps, err := db.ListAgentSteps(acmeCtx, acmeRun, actionBrain)
	if err != nil {
		t.Fatalf("acme ListAgentSteps(own run): %v", err)
	}

	if len(ownSteps) != 1 {
		t.Fatalf("acme saw %d steps of its own run, want 1", len(ownSteps))
	}

	// Globex cannot read acme's transcript: the run belongs to another tenant.
	globexSteps, err := db.ListAgentSteps(globexCtx, acmeRun, actionBrain)
	if err != nil {
		t.Fatalf("globex ListAgentSteps(acme run): %v", err)
	}

	if len(globexSteps) != 0 {
		t.Fatalf("globex saw %d steps of the acme run, want 0", len(globexSteps))
	}
}

func testPruneKeepsSuggestionsWithoutRunWindow(t *testing.T, openFn OpenFunc) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	seedSuggestion(t, db, "sug-kept", store.SuggestionOpen, time.Now().Add(-365*24*time.Hour))

	stats, err := db.Prune(ctx, store.PruneOptions{OutputsOlderThan: time.Hour})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	if stats.SuggestionsDeleted != 0 {
		t.Fatalf("SuggestionsDeleted = %d, want 0 when RunsOlderThan is unset", stats.SuggestionsDeleted)
	}

	if _, err := db.GetSuggestion(ctx, "sug-kept"); err != nil {
		t.Fatalf("suggestion deleted by outputs-only sweep: %v", err)
	}
}
