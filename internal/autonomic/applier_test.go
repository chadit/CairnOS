package autonomic_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/autonomic"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/sqlite"
)

// Shared fixtures; goconst applies to tests in this repo's lint policy.
const (
	workflowName    = "cache_me"
	workflowRunner  = "openai"
	suggestionIDOne = "sug-1"
)

// minimalWorkflow is a valid single-action workflow the applier can find and
// annotate. The annotation is appended as TOML comments, so the file still
// parses after apply.
const minimalWorkflow = `[workflow]
name        = "cache_me"
version     = "1"
description = "a workflow whose repeated action a caching suggestion targets."

[action.fetch]
runner = "openai"

[action.fetch.params]
prompt = "hello"
`

// fixture wires a fresh sqlite store and a workflows directory holding the
// minimal workflow, returning the applier under examination and the workflow
// path so each case can inspect the file after apply or revert.
type fixture struct {
	db           store.Store
	applier      *autonomic.Applier
	workflowPath string
}

// newFixture builds a fixture with auto-apply enabled (the gate-on case); a
// case wanting the default-off path passes enabled = false.
func newFixture(t *testing.T, enabled bool) *fixture {
	t.Helper()

	dir := t.TempDir()
	workflowsDir := filepath.Join(dir, "workflows")

	if err := os.MkdirAll(workflowsDir, 0o750); err != nil {
		t.Fatalf("make workflows dir: %v", err)
	}

	workflowPath := filepath.Join(workflowsDir, "cache_me.toml")
	if err := os.WriteFile(workflowPath, []byte(minimalWorkflow), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	db, err := sqlite.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("close store: %v", closeErr)
		}
	})

	applier := autonomic.New(db, autonomic.Config{
		AutoApplyEnabled: enabled,
		WorkflowsDir:     workflowsDir,
		Actor:            "test",
		Now:              fixedClock(),
	})

	return &fixture{db: db, applier: applier, workflowPath: workflowPath}
}

// fixedClock returns a clock pinned to a known instant so audit and version
// timestamps are reproducible.
func fixedClock() func() time.Time {
	instant := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)

	return func() time.Time { return instant }
}

// seedApproved stores an approved suggestion of the given kind targeting the
// minimal workflow, the precondition Apply needs.
func seedApproved(t *testing.T, db store.Store, id string, kind store.SuggestionKind) {
	t.Helper()

	err := db.SaveSuggestion(t.Context(), &store.Suggestion{
		ID:        id,
		Kind:      kind,
		Target:    workflowName + ".fetch",
		Rationale: "repeated identical call",
		Evidence:  map[string]any{"workflow": workflowName, "action": "fetch", "runner": workflowRunner},
		Status:    store.SuggestionApproved,
		CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("seed suggestion %s: %v", id, err)
	}
}

// TestApplyDisabledByDefault proves the master switch: with auto_apply_enabled
// false, Apply refuses even an approved caching suggestion and touches nothing.
func TestApplyDisabledByDefault(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, false)
	seedApproved(t, fix.db, suggestionIDOne, store.SuggestionCaching)

	_, err := fix.applier.Apply(t.Context(), suggestionIDOne)
	if !errors.Is(err, autonomic.ErrAutoApplyDisabled) {
		t.Fatalf("apply error = %v, want ErrAutoApplyDisabled", err)
	}

	original, _ := os.ReadFile(fix.workflowPath)
	if string(original) != minimalWorkflow {
		t.Error("disabled apply still modified the workflow file")
	}
}

// TestApplyCachingRecordsRollbackAndAnnotates proves the happy path: an
// approved caching suggestion captures the prior version (the rollback point),
// annotates the file, writes an audit row, and marks the suggestion applied.
func TestApplyCachingRecordsRollbackAndAnnotates(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, true)
	seedApproved(t, fix.db, suggestionIDOne, store.SuggestionCaching)

	result, err := fix.applier.Apply(t.Context(), suggestionIDOne)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	// The rollback point is the prior file content, fetchable by its hash.
	version, err := fix.db.GetWorkflowVersion(t.Context(), result.PriorHash)
	if err != nil {
		t.Fatalf("get rollback version: %v", err)
	}

	if version.DefinitionTOML != minimalWorkflow {
		t.Error("captured rollback version does not match the prior file content")
	}

	// The file now carries the annotation marker and still parses.
	updated, _ := os.ReadFile(fix.workflowPath)
	if !strings.Contains(string(updated), "cairnos auto-apply") {
		t.Error("apply did not annotate the workflow file")
	}

	// An audit row records the apply.
	audits, err := fix.db.ListAudit(t.Context(), store.AuditFilter{})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}

	if len(audits) != 1 || audits[0].Action != autonomic.AuditActionApply {
		t.Errorf("audit rows = %+v, want one auto_apply row", audits)
	}

	// The suggestion is now applied.
	suggestion, err := fix.db.GetSuggestion(t.Context(), suggestionIDOne)
	if err != nil {
		t.Fatalf("get suggestion: %v", err)
	}

	if suggestion.Status != store.SuggestionApplied {
		t.Errorf("suggestion status = %q, want applied", suggestion.Status)
	}
}

// TestApplyRefusesParamTuning proves the kind gate: a param-tuning suggestion
// never auto-applies, because verifying it kept output quality is the open R1
// problem.
func TestApplyRefusesParamTuning(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, true)
	seedApproved(t, fix.db, suggestionIDOne, store.SuggestionParamTuning)

	_, err := fix.applier.Apply(t.Context(), suggestionIDOne)
	if !errors.Is(err, autonomic.ErrKindNotAutoApplicable) {
		t.Fatalf("apply error = %v, want ErrKindNotAutoApplicable", err)
	}
}

// TestApplyRefusesUnapproved proves the approval gate: an open suggestion is
// not applied even when enabled and output-neutral.
func TestApplyRefusesUnapproved(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, true)

	err := fix.db.SaveSuggestion(t.Context(), &store.Suggestion{
		ID:        suggestionIDOne,
		Kind:      store.SuggestionCaching,
		Target:    workflowName + ".fetch",
		Evidence:  map[string]any{"workflow": workflowName},
		Status:    store.SuggestionOpen,
		CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("seed open suggestion: %v", err)
	}

	_, applyErr := fix.applier.Apply(t.Context(), suggestionIDOne)
	if !errors.Is(applyErr, autonomic.ErrSuggestionNotApproved) {
		t.Fatalf("apply error = %v, want ErrSuggestionNotApproved", applyErr)
	}
}

// TestRevertRestoresPriorVersion proves the rollback model end to end: apply
// changes the file, revert restores the exact prior bytes and writes an audit
// row.
func TestRevertRestoresPriorVersion(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, true)
	seedApproved(t, fix.db, suggestionIDOne, store.SuggestionCaching)

	if _, err := fix.applier.Apply(t.Context(), suggestionIDOne); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Sanity: the file changed.
	afterApply, _ := os.ReadFile(fix.workflowPath)
	if string(afterApply) == minimalWorkflow {
		t.Fatal("apply did not change the file, so revert proves nothing")
	}

	restored, err := fix.applier.Revert(t.Context(), workflowName)
	if err != nil {
		t.Fatalf("revert: %v", err)
	}

	afterRevert, _ := os.ReadFile(fix.workflowPath)
	if string(afterRevert) != minimalWorkflow {
		t.Errorf("revert did not restore the prior content\ngot:  %q\nwant: %q", afterRevert, minimalWorkflow)
	}

	if restored == "" {
		t.Error("revert reported no restored hash")
	}

	audits, err := fix.db.ListAudit(t.Context(), store.AuditFilter{})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}

	// One apply row and one revert row.
	if len(audits) != 2 {
		t.Fatalf("audit rows = %d, want 2 (apply then revert)", len(audits))
	}

	if audits[0].Action != autonomic.AuditActionRevert {
		t.Errorf("newest audit action = %q, want auto_revert", audits[0].Action)
	}
}

// TestRevertWithoutVersionFails proves a workflow with no captured version
// cannot be reverted.
func TestRevertWithoutVersionFails(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, true)

	_, err := fix.applier.Revert(t.Context(), workflowName)
	if !errors.Is(err, autonomic.ErrNoVersionToRevert) {
		t.Fatalf("revert error = %v, want ErrNoVersionToRevert", err)
	}
}

// TestOutputNeutralClassification pins which kinds auto-apply touches: caching
// and batching, never param tuning.
func TestOutputNeutralClassification(t *testing.T) {
	t.Parallel()

	cases := []struct {
		kind store.SuggestionKind
		want bool
	}{
		{store.SuggestionCaching, true},
		{store.SuggestionBatching, true},
		{store.SuggestionParamTuning, false},
		{"prompt_refactor", false},
	}

	for _, tt := range cases {
		if got := autonomic.OutputNeutral(tt.kind); got != tt.want {
			t.Errorf("OutputNeutral(%q) = %v, want %v", tt.kind, got, tt.want)
		}
	}
}

// TestApplyThenReannotateReplacesBlock proves re-applying replaces the
// annotation block rather than stacking, so the file does not accumulate blocks
// across applies.
func TestApplyThenReannotateReplacesBlock(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, true)
	seedApproved(t, fix.db, suggestionIDOne, store.SuggestionCaching)

	if _, err := fix.applier.Apply(t.Context(), suggestionIDOne); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// Approve a second caching suggestion and apply it over the annotated file.
	seedApproved(t, fix.db, "sug-2", store.SuggestionCaching)

	if _, err := fix.applier.Apply(t.Context(), "sug-2"); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	updated, _ := os.ReadFile(fix.workflowPath)
	if got := strings.Count(string(updated), "cairnos auto-apply"); got != 2 {
		// The marker text appears twice per block (begin and end). Two markers
		// means exactly one block, so re-apply replaced rather than stacked.
		t.Errorf("annotation marker count = %d, want 2 (one block, begin+end)", got)
	}
}
