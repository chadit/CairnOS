package cli

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
)

func (a *App) cmdPrune(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("prune", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	olderFlag := flags.String("older-than", "", "delete runs older than this, e.g. 90d (default: retention.runs from config)")
	outputsFlag := flags.String("outputs-older-than", "", "truncate outputs older than this, e.g. 30d (default: retention.outputs)")
	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	db, cfg, code, proceed := openAt(ctx, a, "prune", *dirFlag, runStoreBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	runsAge, err := pruneAge(*olderFlag, cfg.Retention.Runs)
	if err != nil {
		a.printErrf("prune: --older-than: %v\n", err)

		return ExitUsage
	}

	outputsAge, err := pruneAge(*outputsFlag, cfg.Retention.Outputs)
	if err != nil {
		a.printErrf("prune: --outputs-older-than: %v\n", err)

		return ExitUsage
	}

	stats, err := db.Prune(ctx, store.PruneOptions{RunsOlderThan: runsAge, OutputsOlderThan: outputsAge})
	if err != nil {
		a.printErrf("prune: %v\n", err)

		return ExitFailure
	}

	a.printOutf("deleted %d run(s), truncated %d output(s), deleted %d stale suggestion(s)\n",
		stats.RunsDeleted, stats.OutputsTruncated, stats.SuggestionsDeleted)

	return ExitOK
}

// pruneAge picks the flag value when given, the configured retention window
// otherwise.
func pruneAge(flagValue string, fallback time.Duration) (time.Duration, error) {
	if flagValue == "" {
		return fallback, nil
	}

	age, err := workflow.ParseDuration(flagValue)
	if err != nil {
		return 0, fmt.Errorf("parse retention age: %w", err)
	}

	return age, nil
}
