package cli

import (
	"context"
	"flag"
	"path/filepath"

	"github.com/chadit/CairnOS/internal/autonomic"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/store"
)

// cmdSuggestionsApply applies an approved, output-neutral suggestion to its
// workflow file, recording a rollback point and an audit row first. It is the
// manual entry to the gated auto-apply path: it still refuses all kinds but
// caching and batching, requires approval, and requires [autonomic]
// auto_apply_enabled. Param tuning and prompt refactor never apply: verifying
// they kept output quality is open R1 (.agents/spec/14-open-decisions.md).
func (a *App) cmdSuggestionsApply(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("suggestions apply", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	id, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if id == "" {
		a.printErrf("usage: cairnos suggestions apply <id> [--dir D]\n")

		return ExitUsage
	}

	db, cfg, code, proceed := openAt(ctx, a, "suggestions apply", *dirFlag, runStoreBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	applier, code, proceed := a.buildApplier(db, &cfg, *dirFlag, "suggestions apply")
	if !proceed {
		return code
	}

	result, err := applier.Apply(ctx, id)
	if err != nil {
		a.printErrf("suggestions apply: %v\n", err)

		return ExitFailure
	}

	a.printOutf("applied %s to workflow %q; rollback point recorded (prior version %s).\n",
		id, result.WorkflowName, result.PriorHash)
	a.printOutf("only caching and batching auto-apply; param tuning and prompt refactor never do (R1).\n")
	a.printOutf("revert with: cairnos suggestions revert %s\n", result.WorkflowName)

	return ExitOK
}

// cmdSuggestionsRevert restores a workflow file to its most recent captured
// version, the rollback the version store exists for. It writes the prior
// definition back and records an audit row.
func (a *App) cmdSuggestionsRevert(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("suggestions revert", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	workflowName, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if workflowName == "" {
		a.printErrf("usage: cairnos suggestions revert <workflow> [--dir D]\n")

		return ExitUsage
	}

	db, cfg, code, proceed := openAt(ctx, a, "suggestions revert", *dirFlag, runStoreBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	// Revert is not gated on auto_apply_enabled: undoing a change must always be
	// available, even after the operator turns auto-apply back off.
	applier, code, proceed := a.buildApplier(db, &cfg, *dirFlag, "suggestions revert")
	if !proceed {
		return code
	}

	restored, err := applier.Revert(ctx, workflowName)
	if err != nil {
		a.printErrf("suggestions revert: %v\n", err)

		return ExitFailure
	}

	a.printOutf("reverted workflow %q to version %s\n", workflowName, restored.Hash)

	if restored.HashChanged() {
		a.printErrf("warning: version %s was captured with different stdin_file text; revert restores only the TOML, so the workflow now hashes to %s\n",
			restored.StoredHash, restored.Hash)
	}

	return ExitOK
}

// buildApplier wires the autonomic applier from the store, config, and the
// workflows directory under the CairnOS home. The label prefixes any directory
// resolution error.
func (a *App) buildApplier(db store.Store, cfg *config.Config, dirFlagValue, label string) (*autonomic.Applier, int, bool) {
	home, err := resolveDir(dirFlagValue, a.envLookup())
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return nil, ExitFailure, false
	}

	applier := autonomic.New(db, autonomic.Config{
		AutoApplyEnabled: cfg.Autonomic.AutoApplyEnabled,
		WorkflowsDir:     filepath.Join(home, workflowsDirName),
		Actor:            triggerCLI,
	})

	return applier, ExitOK, true
}
