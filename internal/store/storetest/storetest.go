// Package storetest is the shared behavioral spec for store.Store
// implementations. RunConformance is one suite that every backend runs
// against a fresh store: the sqlite backend calls it, and the postgres
// backend calls it when a test DSN is configured. One spec, two backends,
// so a behavioral difference between them fails a test instead of going
// unnoticed.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sync"
	"testing"
	"time"

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
	absentID        = "missing"

	transcriptPayload = "transcript payload"
	keyArguments      = "arguments"
	keyTool           = "tool"
	keyRunner         = "runner"
	keyMaxTokens      = "max_tokens"
	evidenceRunner    = "anthropic"

	sugOpenOld  = "sug-open-old"
	sugOpenNew  = "sug-open-new"
	sugApproved = "sug-approved"

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

	// The action-result round trip gives every usage field its own value, so a
	// swapped column shows up as a mismatch.
	retryAttempt      = 2
	usageInputTokens  = 100
	usageOutputTokens = 50
	usageCachedTokens = 10
	usageCostUSD      = 0.25
	usageLatency      = 1500 * time.Millisecond

	// listLimit is below the three seeded runs, so the limit case proves a cap.
	listLimit = 2

	// The agent-step transcript case: step numbers and per-step usage.
	stepToolCall       = 2
	stepFinal          = 3
	driverInputTokens  = 10
	driverOutputTokens = 5
	driverCost         = 0.5
	finalStepCost      = 0.25

	// The suggestion round trip's evidence and estimated savings.
	evidenceMaxTokens = 4096
	savingsCostUSD    = 0.42
	savingsLatency    = 250 * time.Millisecond
)

// Retention cutoffs the prune cases pass in PruneOptions.
const (
	day             = 24 * time.Hour
	retentionWindow = 90 * day
	outputRetention = 30 * day
)

// OpenFunc hands the suite a fresh, empty store. The implementation
// registers cleanup (t.Cleanup with Close) so each subtest gets an isolated
// database. S is the backend's own type, so a backend's opener returns what it
// opened while the suite drives only the store.Store methods.
type OpenFunc[S store.Store] func(t *testing.T) S

// RunConformance runs the full store.Store behavioral suite against the
// store openFn hands out. Each named subtest opens its own store, so they
// are independent and the caller may run the suite under t.Parallel.
func RunConformance[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	cases := []struct {
		run  func(t *testing.T, openFn OpenFunc[S])
		name string
	}{
		{name: "RunActionRoundTrip", run: testRunActionRoundTrip},
		{name: "CreateRunRejectsDuplicateID", run: testCreateRunRejectsDuplicateID},
		{name: "UpsertActionResultRejectsUnknownRun", run: testUpsertActionResultRejectsUnknownRun},
		{name: "ParentRunIDRoundTrip", run: testParentRunIDRoundTrip},
		{name: "UpdateRunStatusStampsTimestamps", run: testUpdateRunStatusStampsTimestamps},
		{name: "RunNotFound", run: testRunNotFound},
		{name: "ListRunsFiltersAndOrdersNewestFirst", run: testListRunsFiltersAndOrdersNewestFirst},
		{name: "WorkspaceIsolatesRunsAndActions", run: testWorkspaceIsolatesRunsAndActions},
		{name: "AgentStepsIsolateByWorkspace", run: testAgentStepsIsolateByWorkspace},
		{name: "SpendSinceIsolatesByWorkspace", run: testSpendSinceIsolatesByWorkspace},
		{name: "ReconcileInterruptedFailsRunningWork", run: testReconcileInterruptedFailsRunningWork},
		{name: "PruneDeletesOldRunsAndTruncatesOldOutputs", run: testPruneRunsAndOutputs},
		{name: "AgentStepsRoundTripInOrder", run: testAgentStepsRoundTripInOrder},
		{name: "ConcurrentUpsertActionResult", run: testConcurrentUpsertActionResult},
		{name: "SuggestionRoundTrip", run: testSuggestionRoundTrip},
		{name: "SaveSuggestionDefaults", run: testSaveSuggestionDefaults},
		{name: "ListSuggestionsFilters", run: testListSuggestionsFilters},
		{name: "UpdateSuggestionStatus", run: testUpdateSuggestionStatus},
		{name: "SuggestionNotFound", run: testSuggestionNotFound},
		{name: "SuggestionIsolatesByWorkspace", run: testSuggestionIsolatesByWorkspace},
		{name: "PruneDeletesStaleSuggestions", run: testPruneDeletesStaleSuggestions},
		{name: "PruneWithoutRunWindowKeepsSuggestions", run: testPruneKeepsSuggestionsWithoutRunWindow},
		{name: "AuditAppendListNewestFirst", run: testAuditAppendListNewestFirst},
		{name: "AuditDefaultsStampIDAndTime", run: testAuditDefaultsStampIDAndTime},
		{name: "WorkflowPauseSetGetClear", run: testWorkflowPauseSetGetClear},
		{name: "WorkflowPauseListNewestFirst", run: testWorkflowPauseListNewestFirst},
		{name: "WorkflowPauseIsolatesByWorkspace", run: testWorkflowPauseIsolatesByWorkspace},
		{name: "AuditIsolatesByWorkspace", run: testAuditIsolatesByWorkspace},
		{name: "SpendSinceSumsCostInWindow", run: testSpendSinceSumsCostInWindow},
		{name: "PruneLeavesAuditAndPauseRows", run: testPruneLeavesAuditAndPauseRows},
		{name: "WorkflowVersionRoundTrip", run: testWorkflowVersionRoundTrip},
		{name: "WorkflowVersionSaveStampsTime", run: testWorkflowVersionSaveStampsTime},
		{name: "WorkflowVersionReSaveKeepsOriginal", run: testWorkflowVersionReSaveKeepsOriginal},
		{name: "WorkflowVersionNotFound", run: testWorkflowVersionNotFound},
		{name: "WorkflowVersionListNewestFirst", run: testWorkflowVersionListNewestFirst},
		{name: "PruneLeavesWorkflowVersions", run: testPruneLeavesWorkflowVersions},
		{name: "ChatSessionRoundTrip", run: testChatSessionRoundTrip},
		{name: "ChatSessionSeedsSystemRole", run: testChatSessionSeedsSystemRole},
		{name: "ChatSessionNotFound", run: testChatSessionNotFound},
		{name: "ChatSessionListNewestFirst", run: testChatSessionListNewestFirst},
		{name: "ChatSessionIsolatesByWorkspace", run: testChatSessionIsolatesByWorkspace},
		{name: "ChatSearchRanksBestMatchFirst", run: testChatSearchRanksBestMatchFirst},
		{name: "ChatSearchFiltersBySession", run: testChatSearchFiltersBySession},
		{name: "ChatSearchLimitsResults", run: testChatSearchLimitsResults},
		{name: "ChatSearchIsolatesByWorkspace", run: testChatSearchIsolatesByWorkspace},
		{name: "ChatSearchTreatsQuerySyntaxLiterally", run: testChatSearchTreatsQuerySyntaxLiterally},
		{name: "ChatSearchEmptyTextReturnsNothing", run: testChatSearchEmptyTextReturnsNothing},
		{name: "ApprovalRoundTrip", run: testApprovalRoundTrip},
		{name: "ApprovalNotFound", run: testApprovalNotFound},
		{name: "ApprovalDecideOnce", run: testApprovalDecideOnce},
		{name: "ApprovalListPending", run: testApprovalListPending},
		{name: "ApprovalIsolatesByWorkspace", run: testApprovalIsolatesByWorkspace},
		{name: "ApprovalHeldSurvivesReconcile", run: testApprovalHeldSurvivesReconcile},
		{name: "BridgeSessionRoundTrip", run: testBridgeSessionRoundTrip},
		{name: "BridgeSessionRebindUpserts", run: testBridgeSessionRebindUpserts},
		{name: "BridgeSessionNotFound", run: testBridgeSessionNotFound},
		{name: "BridgeSessionIsolatesByWorkspace", run: testBridgeSessionIsolatesByWorkspace},
		{name: "MessageThreadRoundTrip", run: testMessageThreadRoundTrip},
		{name: "MessageThreadRebindUpserts", run: testMessageThreadRebindUpserts},
		{name: "MessageThreadIsolatesEveryKeyPart", run: testMessageThreadIsolatesEveryKeyPart},
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

// testCreateRunRejectsDuplicateID proves a second create on a taken id is
// refused with a sentinel the caller can act on, rather than silently
// replacing a run that may already be executing.
func testCreateRunRejectsDuplicateID[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	runID := store.NewID()
	created := time.Date(2026, 6, 1, 12, 30, 15, microFixtureNS, time.UTC)

	createRun(t, db, runID, workflowAlpha, store.RunPending, created)

	err := db.CreateRun(t.Context(), &store.Run{
		ID:           runID,
		WorkflowName: workflowBeta,
		WorkflowHash: hashFixture,
		Status:       store.RunRunning,
		Trigger:      triggerCLI,
		CreatedAt:    created,
	})
	if !errors.Is(err, store.ErrRunExists) {
		t.Fatalf("second CreateRun on %s: error = %v, want ErrRunExists", runID, err)
	}

	// The refusal must leave the original row untouched, so a losing creator
	// cannot rewrite the winner's workflow or status.
	got, getErr := db.GetRun(t.Context(), runID)
	if getErr != nil {
		t.Fatalf("get run %s: %v", runID, getErr)
	}

	if got.WorkflowName != workflowAlpha || got.Status != store.RunPending {
		t.Fatalf("run after refused create = (%s, %s), want (%s, %s)",
			got.WorkflowName, got.Status, workflowAlpha, store.RunPending)
	}
}

// testUpsertActionResultRejectsUnknownRun proves an action write naming a run
// nothing created is refused instead of stranding an orphan row that no run
// query will ever return.
func testUpsertActionResultRejectsUnknownRun[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	missingRun := store.NewID()

	err := db.UpsertActionResult(t.Context(), &store.ActionResult{
		RunID:      missingRun,
		ActionName: actionFetch,
		Runner:     runnerMock,
		Status:     store.ActionSucceeded,
	})
	if !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("UpsertActionResult for unknown run %s: error = %v, want ErrRunNotFound", missingRun, err)
	}
}

func testRunActionRoundTrip[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	// Microsecond precision is the contract both backends honor: Postgres
	// timestamptz is microsecond-resolution, so the suite never asserts
	// sub-microsecond fidelity that sqlite would keep but Postgres would
	// truncate. Real timestamps come from time.Now and round-trip fine.
	created := time.Date(2026, 6, 1, 12, 30, 15, microFixtureNS, time.UTC)
	runID := store.NewID()
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
		Attempt:    retryAttempt,
		Output:     map[string]any{"score": float64(scoreValue), "note": "partial"},
		Usage: runner.Usage{
			InputTokens:  usageInputTokens,
			OutputTokens: usageOutputTokens,
			CachedTokens: usageCachedTokens,
			CostUSD:      usageCostUSD,
			Latency:      usageLatency,
			Retries:      1,
		},
		Meta:       map[string]string{"model": "mock-1", "request_id": "req-9"},
		Error:      cairnerr.New("runner.flaky", "second attempt failed").WithDetail("attempt", "2"),
		StartedAt:  created.Add(time.Second),
		FinishedAt: created.Add(twoUnits * time.Second),
	}

	if upsertActionResultErr := db.UpsertActionResult(ctx, want); upsertActionResultErr != nil {
		t.Fatalf("upsert action result: %v", upsertActionResultErr)
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

	// The row is compared column by column rather than whole. DeepEqual on an
	// ActionResult would compare the error chain behind its *cairnerr.Error,
	// which no store round-trips, and would trip on time.Time internals that
	// differ between backends.
	if !cairnerr.Equal(got.Error, want.Error) {
		t.Errorf("error = %#v, want %#v", got.Error, want.Error)
	}

	if !sameActionColumns(got, want) {
		t.Errorf("action result = %#v, want %#v", got, want)
	}

	if !reflect.DeepEqual(got.Output, want.Output) {
		t.Errorf("output = %#v, want %#v", got.Output, want.Output)
	}

	if !maps.Equal(got.Meta, want.Meta) {
		t.Errorf("meta = %#v, want %#v", got.Meta, want.Meta)
	}
}

// sameActionColumns compares the row's scalar columns. The timestamps were
// already compared with Equal above, and the two fields that need a deep look
// are compared beside the call.
func sameActionColumns(got, want *store.ActionResult) bool {
	return got.RunID == want.RunID &&
		got.ActionName == want.ActionName &&
		got.Runner == want.Runner &&
		got.Status == want.Status &&
		got.Attempt == want.Attempt &&
		got.Usage == want.Usage
}

// testParentRunIDRoundTrip proves runs.parent_run_id survives the store: a
// child run created with ParentRunID set reads it back, while a root run reads
// the empty string. Replay, workflow-call, and error-workflow children all rely
// on this link.
func testParentRunIDRoundTrip[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	parentID := store.NewID()
	childID := store.NewID()
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

func testUpdateRunStatusStampsTimestamps[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	runID := store.NewID()

	createRun(t, db, runID, workflowAlpha, store.RunPending, time.Now())

	run, err := db.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get pending run: %v", err)
	}

	if !run.StartedAt.IsZero() || !run.FinishedAt.IsZero() {
		t.Fatalf("pending run has stamps: started=%v finished=%v", run.StartedAt, run.FinishedAt)
	}

	if updateRunStatusErr := db.UpdateRunStatus(ctx, runID, store.RunRunning, nil); updateRunStatusErr != nil {
		t.Fatalf("transition to running: %v", updateRunStatusErr)
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

	if updateRunStatusErr := db.UpdateRunStatus(ctx, runID, store.RunRunning, nil); updateRunStatusErr != nil {
		t.Fatalf("repeat transition to running: %v", updateRunStatusErr)
	}

	run, err = db.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get re-running run: %v", err)
	}

	if !run.StartedAt.Equal(firstStart) {
		t.Errorf("second running transition moved started_at from %v to %v", firstStart, run.StartedAt)
	}

	failure := cairnerr.New("runner.boom", "exploded")
	if updateRunStatusErr := db.UpdateRunStatus(ctx, runID, store.RunFailed, failure); updateRunStatusErr != nil {
		t.Fatalf("transition to failed: %v", updateRunStatusErr)
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

func testRunNotFound[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	if _, err := db.GetRun(ctx, absentID); !errors.Is(err, store.ErrRunNotFound) {
		t.Errorf("GetRun error = %v, want ErrRunNotFound", err)
	}

	err := db.UpdateRunStatus(ctx, absentID, store.RunRunning, nil)
	if !errors.Is(err, store.ErrRunNotFound) {
		t.Errorf("UpdateRunStatus error = %v, want ErrRunNotFound", err)
	}
}

func testListRunsFiltersAndOrdersNewestFirst[S store.Store](t *testing.T, openFn OpenFunc[S]) {
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
			filter: store.ListFilter{Limit: listLimit},
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

// testWorkspaceIsolatesRunsAndActions is the multi-tenancy invariant: a run
// created in one workspace is invisible to a context scoped to
// another, for GetRun, ListRuns, and ListActionResults. The context carries the
// workspace; the store filters by it.
func testWorkspaceIsolatesRunsAndActions[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	base := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	acmeCtx := store.ContextWithWorkspace(t.Context(), workspaceAcme)
	globexCtx := store.ContextWithWorkspace(t.Context(), workspaceGlobex)

	acmeRun := store.NewID()
	globexRun := store.NewID()

	createRunIn(t, db, acmeCtx, acmeRun, base)
	createRunIn(t, db, globexCtx, globexRun, base.Add(time.Hour))

	upsertActionIn(t, db, acmeCtx, acmeRun)
	upsertActionIn(t, db, globexCtx, globexRun)

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
	if _, getRunErr := db.GetRun(acmeCtx, globexRun); !errors.Is(getRunErr, store.ErrRunNotFound) {
		t.Fatalf("acme GetRun(globex run) error = %v, want ErrRunNotFound", getRunErr)
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
func testSpendSinceIsolatesByWorkspace[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	base := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	acmeCtx := store.ContextWithWorkspace(t.Context(), workspaceAcme)
	globexCtx := store.ContextWithWorkspace(t.Context(), workspaceGlobex)

	acmeRun := store.NewID()
	globexRun := store.NewID()

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

// createRunIn stages a pending run of workflowAlpha, the workflow every
// isolation case shares, in the workspace ctx carries. It stamps WorkspaceID
// the way engine.RunWorkflow does, so the row lands in the tenant the
// isolation cases read back.
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

// upsertActionIn stages one succeeded fetch action on a run in the workspace
// ctx carries, so the isolation test's workspace-scoped reads have a row to
// find.
//
//nolint:revive // t leads every helper here for the t.Fatalf calls; ctx follows to carry the workspace
func upsertActionIn(t *testing.T, db store.Store, ctx context.Context, runID string) {
	t.Helper()

	err := db.UpsertActionResult(ctx, &store.ActionResult{
		RunID:      runID,
		ActionName: actionFetch,
		Runner:     runnerMock,
		Status:     store.ActionSucceeded,
	})
	if err != nil {
		t.Fatalf("upsert action %s/%s: %v", runID, actionFetch, err)
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

func testReconcileInterruptedFailsRunningWork[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	now := time.Date(2026, 5, 2, 9, 0, 0, 0, time.UTC)

	const runDone = "run-done"

	createRun(t, db, runLive, workflowAlpha, store.RunRunning, now)
	createRun(t, db, runDone, workflowAlpha, store.RunSucceeded, now)

	stage := []*store.ActionResult{
		{RunID: runLive, ActionName: actionFetch, Runner: runnerMock, Status: store.ActionRunning},
		{RunID: runLive, ActionName: actionBrain, Runner: runnerMock, Status: store.ActionSucceeded},
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

func testPruneRunsAndOutputs[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

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
			Output:     transcriptPayload,
			CreatedAt:  now,
		}
		if err := db.AppendAgentStep(ctx, step); err != nil {
			t.Fatalf("stage agent step for %s: %v", runID, err)
		}
	}

	stats, err := db.Prune(ctx, store.PruneOptions{
		RunsOlderThan:    retentionWindow,
		OutputsOlderThan: outputRetention,
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

	if len(steps) != 1 || steps[0].Output != transcriptPayload {
		t.Errorf("intact agent steps = %#v, want the transcript output kept", steps)
	}
}

func testAgentStepsRoundTripInOrder[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	runID := store.NewID()

	createRun(t, db, runID, workflowAlpha, store.RunRunning, time.Now())

	staged := []*store.AgentStep{
		{
			RunID:      runID,
			ActionName: actionBrain,
			Step:       1,
			Kind:       store.StepDriverCall,
			Output:     map[string]any{keyTool: runnerMock, keyArguments: map[string]any{paramOutput: echoValue}},
			Usage:      runner.Usage{InputTokens: driverInputTokens, OutputTokens: driverOutputTokens, CostUSD: driverCost},
			CreatedAt:  time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			RunID:      runID,
			ActionName: actionBrain,
			Step:       stepToolCall,
			Kind:       store.StepToolCall,
			Tool:       runnerMock,
			Input:      map[string]any{paramOutput: echoValue},
			Output:     echoValue,
			CreatedAt:  time.Date(2026, 6, 1, 0, 0, 1, 0, time.UTC),
		},
		{
			RunID:      runID,
			ActionName: actionBrain,
			Step:       stepFinal,
			Kind:       store.StepFinal,
			Output:     "done",
			Usage:      runner.Usage{CostUSD: finalStepCost},
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
	wantOutput := map[string]any{keyTool: runnerMock, keyArguments: map[string]any{paramOutput: echoValue}}
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

func testConcurrentUpsertActionResult[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()
	runID := store.NewID()

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

func testSuggestionRoundTrip[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	created := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	want := &store.Suggestion{
		ID:        store.NewID(),
		Kind:      store.SuggestionParamTuning,
		Target:    suggestionTarget,
		Rationale: suggestionRationale,
		Evidence: map[string]any{
			keyRunner:    evidenceRunner,
			keyMaxTokens: float64(evidenceMaxTokens),
		},
		Proposed: map[string]any{
			keyMaxTokens: float64(proposedTokens),
		},
		EstSavings: store.Savings{
			CostUSDPerRun: savingsCostUSD,
			LatencyDelta:  savingsLatency,
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

	if got.Evidence[keyRunner] != evidenceRunner || got.Evidence[keyMaxTokens] != float64(evidenceMaxTokens) {
		t.Fatalf("evidence = %+v, want runner and max_tokens preserved", got.Evidence)
	}

	if got.Proposed[keyMaxTokens] != float64(proposedTokens) {
		t.Fatalf("proposed = %+v, want max_tokens 128", got.Proposed)
	}

	if !got.CreatedAt.Equal(created) {
		t.Fatalf("created_at = %v, want %v", got.CreatedAt, created)
	}
}

func testSaveSuggestionDefaults[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	suggestion := &store.Suggestion{
		ID:        store.NewID(),
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

func testListSuggestionsFilters[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	seedSuggestion(t, db, sugOpenOld, store.SuggestionOpen, base)
	seedSuggestion(t, db, sugOpenNew, store.SuggestionOpen, base.Add(time.Hour))
	seedSuggestion(t, db, sugApproved, store.SuggestionApproved, base.Add(twoUnits*time.Hour))

	tests := []struct {
		name    string
		filter  store.SuggestionFilter
		wantIDs []string
	}{
		{
			name:    "no filter lists newest first",
			filter:  store.SuggestionFilter{},
			wantIDs: []string{sugApproved, sugOpenNew, sugOpenOld},
		},
		{
			name:    "status filter narrows to open",
			filter:  store.SuggestionFilter{Status: store.SuggestionOpen},
			wantIDs: []string{sugOpenNew, sugOpenOld},
		},
		{
			name:    "limit caps the listing",
			filter:  store.SuggestionFilter{Limit: 1},
			wantIDs: []string{sugApproved},
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

func testUpdateSuggestionStatus[S store.Store](t *testing.T, openFn OpenFunc[S]) {
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

func testSuggestionNotFound[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	if _, err := db.GetSuggestion(ctx, absentID); !errors.Is(err, store.ErrSuggestionNotFound) {
		t.Fatalf("get missing suggestion err = %v, want ErrSuggestionNotFound", err)
	}

	err := db.UpdateSuggestionStatus(ctx, absentID, store.SuggestionRejected)
	if !errors.Is(err, store.ErrSuggestionNotFound) {
		t.Fatalf("update missing suggestion err = %v, want ErrSuggestionNotFound", err)
	}
}

func testPruneDeletesStaleSuggestions[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	ctx := t.Context()

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

	seedSuggestion(t, db, "sug-stale", store.SuggestionOpen, now.Add(-retentionWindow-time.Hour))
	seedSuggestion(t, db, "sug-stale-approved", store.SuggestionApproved, now.Add(-retentionWindow-2*time.Hour))
	seedSuggestion(t, db, "sug-fresh", store.SuggestionOpen, now.Add(-time.Hour))

	stats, err := db.Prune(ctx, store.PruneOptions{RunsOlderThan: retentionWindow, Now: now})
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
// scoped to the request tenant. A non-admin token in one tenant must not read
// another's suggestion, which embeds workflow names and costs.
func testSuggestionIsolatesByWorkspace[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	created := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	acmeCtx := store.ContextWithWorkspace(t.Context(), workspaceAcme)
	globexCtx := store.ContextWithWorkspace(t.Context(), workspaceGlobex)

	acmeID := store.NewID()
	globexID := store.NewID()

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
// for a run in workspace A read as empty under a workspace-B context.
func testAgentStepsIsolateByWorkspace[S store.Store](t *testing.T, openFn OpenFunc[S]) {
	t.Helper()

	db := openFn(t)
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	acmeCtx := store.ContextWithWorkspace(t.Context(), workspaceAcme)
	globexCtx := store.ContextWithWorkspace(t.Context(), workspaceGlobex)

	acmeRun := store.NewID()
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

func testPruneKeepsSuggestionsWithoutRunWindow[S store.Store](t *testing.T, openFn OpenFunc[S]) {
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
