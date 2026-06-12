package cli

import (
	"context"
	"flag"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
)

const costUsage = "usage: cairnos cost [--since 7d] [--workflow X] [--dir D]\n"

// costRow aggregates usage for one workflow.
type costRow struct {
	runs         int
	actions      int
	inputTokens  int64
	outputTokens int64
	cachedTokens int64
	retries      int
	costUSD      float64
}

// cmdCost aggregates per-run Usage from action results into a per-workflow
// cost table, optionally bounded by a --since window.
func (a *App) cmdCost(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("cost", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	sinceFlag := flags.String("since", "", "only count runs created within this window (e.g. 7d, 12h)")
	workflowFlag := flags.String("workflow", "", "narrow to one workflow name")
	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	if flags.NArg() > 0 {
		a.printErrf("%s", costUsage)

		return ExitUsage
	}

	var cutoff time.Time

	if *sinceFlag != "" {
		window, err := workflow.ParseDuration(*sinceFlag)
		if err != nil {
			a.printErrf("cost: --since: %v\n", err)

			return ExitUsage
		}

		cutoff = time.Now().Add(-window)
	}

	db, _, code, proceed := a.openStoreAt(ctx, "cost", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	rows, err := aggregateCost(ctx, db, *workflowFlag, cutoff)
	if err != nil {
		a.printErrf("cost: %v\n", err)

		return ExitFailure
	}

	a.writeCostTable(rows)

	return ExitOK
}

// aggregateCost folds every matching run's action usage into per-workflow
// rows. Action results load run by run: the store interface has no joined
// usage query yet, the run list is already bounded by the filter, and one
// indexed lookup per run keeps this read path simple until a reporting
// query earns its place in the interface.
func aggregateCost(ctx context.Context, db store.Store, workflowName string, cutoff time.Time) (map[string]*costRow, error) {
	runs, err := db.ListRuns(ctx, store.ListFilter{WorkflowName: workflowName})
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}

	// Capacity hint: at most one row per run's workflow name.
	rows := make(map[string]*costRow, len(runs))

	for _, run := range runs {
		if !cutoff.IsZero() && run.CreatedAt.Before(cutoff) {
			continue
		}

		results, err := db.ListActionResults(ctx, run.ID)
		if err != nil {
			return nil, fmt.Errorf("list action results for run %s: %w", run.ID, err)
		}

		row, exists := rows[run.WorkflowName]
		if !exists {
			row = &costRow{}
			rows[run.WorkflowName] = row
		}

		row.runs++

		for _, result := range results {
			row.actions++
			row.inputTokens += result.Usage.InputTokens
			row.outputTokens += result.Usage.OutputTokens
			row.cachedTokens += result.Usage.CachedTokens
			row.retries += result.Usage.Retries
			row.costUSD += result.Usage.CostUSD
		}
	}

	return rows, nil
}

// writeCostTable renders the per-workflow rows plus a total line.
func (a *App) writeCostTable(rows map[string]*costRow) {
	if len(rows) == 0 {
		a.printOutf("no runs matched\n")

		return
	}

	tab := newTab(a.stdout)

	a.tabWritef(tab, "WORKFLOW\tRUNS\tACTIONS\tINPUT TOK\tOUTPUT TOK\tCACHED TOK\tRETRIES\tCOST\n")

	total := costRow{}

	for _, name := range slices.Sorted(maps.Keys(rows)) {
		row := rows[name]

		a.tabWritef(tab, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t$%.4f\n",
			name, row.runs, row.actions, row.inputTokens, row.outputTokens,
			row.cachedTokens, row.retries, row.costUSD)

		total.runs += row.runs
		total.actions += row.actions
		total.inputTokens += row.inputTokens
		total.outputTokens += row.outputTokens
		total.cachedTokens += row.cachedTokens
		total.retries += row.retries
		total.costUSD += row.costUSD
	}

	a.tabWritef(tab, "TOTAL\t%d\t%d\t%d\t%d\t%d\t%d\t$%.4f\n",
		total.runs, total.actions, total.inputTokens, total.outputTokens,
		total.cachedTokens, total.retries, total.costUSD)

	a.flushTab(tab)
}
