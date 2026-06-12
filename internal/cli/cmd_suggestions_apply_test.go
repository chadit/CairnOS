package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/store"
)

const applyWorkflowName = "cache_me"

// cacheWorkflow is a valid single-action workflow the apply command can find
// and annotate.
const cacheWorkflow = `[workflow]
name        = "cache_me"
version     = "1"
description = "a workflow with a repeated action."

[action.fetch]
runner = "openai"

[action.fetch.params]
prompt = "hello"
`

// writeAutonomicConfig writes a config enabling auto-apply (or not).
func writeAutonomicConfig(t *testing.T, dir string, enabled bool) {
	t.Helper()

	cfg := "[autonomic]\nauto_apply_enabled = " + boolText(enabled) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write autonomic config: %v", err)
	}
}

func boolText(value bool) string {
	if value {
		return "true"
	}

	return "false"
}

// writeApplyWorkflow drops the cache_me workflow into the home and seeds an
// approved caching suggestion targeting it.
func writeApplyWorkflow(t *testing.T, dir, suggestionID string) {
	t.Helper()

	path := filepath.Join(dir, "workflows", "cache_me.toml")
	if err := os.WriteFile(path, []byte(cacheWorkflow), 0o600); err != nil {
		t.Fatalf("write apply workflow: %v", err)
	}

	db := openRunStore(t, dir)

	err := db.SaveSuggestion(t.Context(), &store.Suggestion{
		ID:        suggestionID,
		Kind:      store.SuggestionCaching,
		Target:    applyWorkflowName + ".fetch",
		Rationale: "repeated identical call",
		Evidence:  map[string]any{"workflow": applyWorkflowName, "action": "fetch", "runner": "openai"},
		Status:    store.SuggestionApproved,
		CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("seed approved suggestion: %v", err)
	}
}

// TestSuggestionsApplyDisabledByDefault proves the CLI honors the master
// switch: with auto-apply off, apply refuses.
func TestSuggestionsApplyDisabledByDefault(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	writeAutonomicConfig(t, dir, false)
	writeApplyWorkflow(t, dir, "apply-1")

	code, _, stderr := runCLI(t, suggestionsCmd, "apply", "apply-1", flagDirArg, dir)
	if code == cli.ExitOK {
		t.Fatal("apply succeeded with auto-apply disabled")
	}

	if !strings.Contains(stderr, "disabled") {
		t.Errorf("apply error did not mention the disabled gate: %s", stderr)
	}
}

// TestSuggestionsApplyThenRevert proves the full manual apply-and-rollback
// loop: apply annotates the file and records a rollback point, revert restores
// the exact prior bytes.
func TestSuggestionsApplyThenRevert(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	writeAutonomicConfig(t, dir, true)
	writeApplyWorkflow(t, dir, "apply-1")

	workflowPath := filepath.Join(dir, "workflows", "cache_me.toml")

	code, stdout, stderr := runCLI(t, suggestionsCmd, "apply", "apply-1", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("apply exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "rollback point") {
		t.Errorf("apply output did not mention the rollback point: %s", stdout)
	}

	afterApply, _ := os.ReadFile(workflowPath)
	if !strings.Contains(string(afterApply), "cairnos auto-apply") {
		t.Error("apply did not annotate the workflow file")
	}

	// The suggestion is now applied.
	db := openRunStore(t, dir)

	suggestion, err := db.GetSuggestion(t.Context(), "apply-1")
	if err != nil {
		t.Fatalf("get suggestion: %v", err)
	}

	if suggestion.Status != store.SuggestionApplied {
		t.Errorf("suggestion status = %q, want applied", suggestion.Status)
	}

	// Revert restores the prior content.
	code, _, stderr = runCLI(t, suggestionsCmd, "revert", applyWorkflowName, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("revert exited %d: %s", code, stderr)
	}

	afterRevert, _ := os.ReadFile(workflowPath)
	if string(afterRevert) != cacheWorkflow {
		t.Errorf("revert did not restore the original content\ngot:  %q\nwant: %q", afterRevert, cacheWorkflow)
	}
}

// TestSuggestionsApplyRefusesParamTuning proves the kind gate at the CLI layer:
// a param-tuning suggestion never auto-applies even with the switch on.
func TestSuggestionsApplyRefusesParamTuning(t *testing.T) {
	t.Parallel()

	dir := initDir(t)
	writeAutonomicConfig(t, dir, true)

	path := filepath.Join(dir, "workflows", "cache_me.toml")
	if err := os.WriteFile(path, []byte(cacheWorkflow), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	db := openRunStore(t, dir)

	err := db.SaveSuggestion(t.Context(), &store.Suggestion{
		ID:        "param-1",
		Kind:      store.SuggestionParamTuning,
		Target:    applyWorkflowName + ".fetch",
		Evidence:  map[string]any{"workflow": applyWorkflowName},
		Status:    store.SuggestionApproved,
		CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("seed param suggestion: %v", err)
	}

	code, _, stderr := runCLI(t, suggestionsCmd, "apply", "param-1", flagDirArg, dir)
	if code == cli.ExitOK {
		t.Fatal("apply accepted a param-tuning suggestion")
	}

	if !strings.Contains(stderr, "output-neutral") {
		t.Errorf("apply error did not name the output-neutral gate: %s", stderr)
	}
}
