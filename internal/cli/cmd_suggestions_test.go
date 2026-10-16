package cli_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Suggestion-flow fixture literals.
const (
	suggestionsCmd = "suggestions"
	repeatWorkflow = "repeat-wf"
	repeatAction   = "fetch"
	repeatOutput   = "same answer"

	// cachingRationale and cachingRunner fill the caching suggestions the
	// apply and hive tests seed straight into the store.
	cachingRationale = "repeated identical call"
	cachingRunner    = "openai"
)

// seedRepeatRuns stages runCount runs whose fetch action always saw the same
// inputs and produced the same output, the caching detector's signal.
func seedRepeatRuns(t *testing.T, home string, runCount int) {
	t.Helper()

	db := openRunStore(t, home)
	ctx := t.Context()
	base := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	inputs := map[string]any{"query": "jobs"}

	for idx := range runCount {
		runID := fmt.Sprintf("repeat-run-%d", idx)
		created := base.Add(time.Duration(idx) * time.Hour)

		err := db.CreateRun(ctx, &store.Run{
			ID:           runID,
			WorkflowName: repeatWorkflow,
			WorkflowHash: "hash-repeat",
			Status:       store.RunSucceeded,
			Inputs:       inputs,
			Trigger:      cliTrigger,
			CreatedAt:    created,
		})
		if err != nil {
			t.Fatalf("seed run %s: %v", runID, err)
		}

		err = db.UpsertActionResult(ctx, &store.ActionResult{
			RunID:      runID,
			ActionName: repeatAction,
			Runner:     "mock",
			Status:     store.ActionSucceeded,
			Output:     repeatOutput,
			Usage:      runner.Usage{CostUSD: 0.10, Latency: time.Second},
			StartedAt:  created.Add(time.Second),
			FinishedAt: created.Add(2 * time.Second),
		})
		if err != nil {
			t.Fatalf("seed action %s: %v", runID, err)
		}
	}
}

// openSuggestionIDs reads the persisted suggestion IDs straight from the
// home's store.
func openSuggestionIDs(t *testing.T, home string, status store.SuggestionStatus) []string {
	t.Helper()

	db := openRunStore(t, home)

	listed, err := db.ListSuggestions(t.Context(), store.SuggestionFilter{Status: status})
	if err != nil {
		t.Fatalf("list suggestions: %v", err)
	}

	ids := make([]string, 0, len(listed))
	for _, suggestion := range listed {
		ids = append(ids, suggestion.ID)
	}

	return ids
}

func TestSuggestionsScanListApproveFlow(t *testing.T) {
	t.Parallel()

	home := initDir(t)
	seedRepeatRuns(t, home, 4)

	code, stdout, stderr := runCLI(t, suggestionsCmd, "scan", flagDirArg, home)
	if code != cli.ExitOK {
		t.Fatalf("scan exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, string(store.SuggestionCaching)) || !strings.Contains(stdout, repeatWorkflow+"."+repeatAction) {
		t.Fatalf("scan output missing the caching suggestion:\n%s", stdout)
	}

	openIDs := openSuggestionIDs(t, home, store.SuggestionOpen)
	if len(openIDs) != 1 {
		t.Fatalf("persisted %d open suggestions, want 1", len(openIDs))
	}

	id := openIDs[0]

	code, stdout, stderr = runCLI(t, suggestionsCmd, "list", flagDirArg, home)
	if code != cli.ExitOK {
		t.Fatalf("list exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, id) {
		t.Fatalf("list output missing suggestion %s:\n%s", id, stdout)
	}

	code, stdout, _ = runCLI(t, suggestionsCmd, "list", "--status", "approved", flagDirArg, home)
	if code != cli.ExitOK || !strings.Contains(stdout, "no suggestions") {
		t.Fatalf("approved filter should be empty before review, got exit %d:\n%s", code, stdout)
	}

	code, stdout, stderr = runCLI(t, suggestionsCmd, "approve", id, flagDirArg, home)
	if code != cli.ExitOK {
		t.Fatalf("approve exited %d: %s", code, stderr)
	}

	// Approval hands the human an annotated change and writes nothing; the
	// printed text must say both.
	if !strings.Contains(stdout, "approved "+id) || !strings.Contains(stdout, "apply this by hand") {
		t.Fatalf("approve output missing the annotation contract:\n%s", stdout)
	}

	if !strings.Contains(stdout, "cache_ttl") {
		t.Fatalf("approve output missing the proposed change:\n%s", stdout)
	}

	if approved := openSuggestionIDs(t, home, store.SuggestionApproved); len(approved) != 1 {
		t.Fatalf("approved suggestions = %v, want exactly the reviewed one", approved)
	}
}

func TestSuggestionsRejectFlow(t *testing.T) {
	t.Parallel()

	home := initDir(t)
	seedRepeatRuns(t, home, 4)

	if code, _, stderr := runCLI(t, suggestionsCmd, "scan", flagDirArg, home); code != cli.ExitOK {
		t.Fatalf("scan exited %d: %s", code, stderr)
	}

	openIDs := openSuggestionIDs(t, home, store.SuggestionOpen)
	if len(openIDs) != 1 {
		t.Fatalf("persisted %d open suggestions, want 1", len(openIDs))
	}

	code, stdout, stderr := runCLI(t, suggestionsCmd, "reject", openIDs[0], flagDirArg, home)
	if code != cli.ExitOK {
		t.Fatalf("reject exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "rejected "+openIDs[0]) {
		t.Fatalf("reject output missing confirmation:\n%s", stdout)
	}

	if rejected := openSuggestionIDs(t, home, store.SuggestionRejected); len(rejected) != 1 {
		t.Fatalf("rejected suggestions = %v, want exactly the reviewed one", rejected)
	}
}

func TestSuggestionsScanBelowFloorPersistsNothing(t *testing.T) {
	t.Parallel()

	home := initDir(t)

	// Two repeats sit under the caching detector's threshold of three, so
	// the scan must come back empty rather than padding the queue.
	seedRepeatRuns(t, home, 2)

	code, stdout, stderr := runCLI(t, suggestionsCmd, "scan", flagDirArg, home)
	if code != cli.ExitOK {
		t.Fatalf("scan exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "no suggestions cleared the savings floor") {
		t.Fatalf("scan output should report an empty result:\n%s", stdout)
	}

	if ids := openSuggestionIDs(t, home, ""); len(ids) != 0 {
		t.Fatalf("persisted %v, want nothing below the detection thresholds", ids)
	}
}

func TestSuggestionsScanWithReplayFlagOnOutputNeutralKinds(t *testing.T) {
	t.Parallel()

	home := initDir(t)
	seedRepeatRuns(t, home, 4)

	// Caching is output-neutral, so --replay has nothing to re-execute and
	// the scan must complete without touching any runner.
	code, stdout, stderr := runCLI(t, suggestionsCmd, "scan", "--replay", flagDirArg, home)
	if code != cli.ExitOK {
		t.Fatalf("scan --replay exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, string(store.SuggestionCaching)) {
		t.Fatalf("scan --replay output missing the caching suggestion:\n%s", stdout)
	}
}

func TestSuggestionsUsageErrors(t *testing.T) {
	t.Parallel()

	home := initDir(t)

	tests := []struct {
		name string
		args []string
	}{
		{name: "no subcommand", args: []string{suggestionsCmd}},
		{name: "unknown subcommand", args: []string{suggestionsCmd, "bogus"}},
		{name: "approve without id", args: []string{suggestionsCmd, "approve", flagDirArg, home}},
		{name: "list with invalid status", args: []string{suggestionsCmd, "list", "--status", "maybe", flagDirArg, home}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if code, _, _ := runCLI(t, tt.args...); code != cli.ExitUsage {
				t.Fatalf("exit = %d, want usage error", code)
			}
		})
	}
}

func TestSuggestionsApproveUnknownID(t *testing.T) {
	t.Parallel()

	home := initDir(t)

	code, _, stderr := runCLI(t, suggestionsCmd, "approve", sqlite.NewID(), flagDirArg, home)
	if code != cli.ExitFailure {
		t.Fatalf("approve of a missing id exited %d, want failure", code)
	}

	if !strings.Contains(stderr, "suggestion not found") {
		t.Fatalf("stderr missing the not-found reason: %s", stderr)
	}
}
