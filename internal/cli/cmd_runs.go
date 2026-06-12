package cli

import (
	"context"
	"flag"

	"github.com/chadit/CairnOS/internal/store"
)

const runsUsage = "usage: cairnos runs <list|show> [flags]\n"

func (a *App) cmdRuns(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printErrf("%s", runsUsage)

		return ExitUsage
	}

	switch args[0] {
	case subcmdList:
		return a.cmdRunsList(ctx, args[1:])
	case "show":
		return a.cmdRunsShow(ctx, args[1:])
	default:
		a.printErrf("unknown runs subcommand %q\n", args[0])
		a.printErrf("%s", runsUsage)

		return ExitUsage
	}
}

func (a *App) cmdRunsList(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("runs list", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	workflowFlag := flags.String("workflow", "", "only runs of this workflow")
	statusFlag := flags.String("status", "", "only runs in this status (pending|running|succeeded|failed|canceled)")
	limitFlag := flags.Int("limit", defaultListLimit, "maximum rows")
	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	db, _, code, proceed := a.openStoreAt(ctx, "runs list", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	runs, err := db.ListRuns(ctx, store.ListFilter{
		WorkflowName: *workflowFlag,
		Status:       store.RunStatus(*statusFlag),
		Limit:        *limitFlag,
	})
	if err != nil {
		a.printErrf("runs list: %v\n", err)

		return ExitFailure
	}

	tab := newTab(a.stdout)

	a.tabWritef(tab, "ID\tWORKFLOW\tSTATUS\tTRIGGER\tCREATED\tFINISHED\n")

	for _, run := range runs {
		a.tabWritef(tab, "%s\t%s\t%s\t%s\t%s\t%s\n",
			run.ID, run.WorkflowName, run.Status, run.Trigger,
			formatTableTime(run.CreatedAt), formatTableTime(run.FinishedAt))
	}

	a.flushTab(tab)

	return ExitOK
}

func (a *App) cmdRunsShow(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("runs show", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	runID, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if runID == "" {
		a.printErrf("usage: cairnos runs show <run-id> [--dir D]\n")

		return ExitUsage
	}

	db, _, code, proceed := a.openStoreAt(ctx, "runs show", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	run, err := db.GetRun(ctx, runID)
	if err != nil {
		a.printErrf("runs show: %v\n", err)

		return ExitFailure
	}

	results, err := db.ListActionResults(ctx, runID)
	if err != nil {
		a.printErrf("runs show: %v\n", err)

		return ExitFailure
	}

	a.printRunDetail(run)
	a.printOutf("\n")
	a.writeActionTable(results)

	return ExitOK
}

// printRunDetail renders the run header fields.
func (a *App) printRunDetail(run *store.Run) {
	a.printOutf("Run:      %s\n", run.ID)
	a.printOutf("Workflow: %s\n", run.WorkflowName)

	if run.WorkflowHash != "" {
		a.printOutf("Hash:     %s\n", run.WorkflowHash)
	}

	a.printOutf("Status:   %s\n", run.Status)
	a.printOutf("Trigger:  %s\n", run.Trigger)
	a.printOutf("Created:  %s\n", formatTableTime(run.CreatedAt))
	a.printOutf("Finished: %s\n", formatTableTime(run.FinishedAt))

	if run.Error != nil {
		a.printOutf("Error:    %s: %s\n", run.Error.Code, run.Error.Message)
	}
}
