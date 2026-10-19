package cli

import (
	"context"
	"flag"
	"path/filepath"
	"time"

	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
)

// actorCLI labels governance actions taken from the command line in the audit
// log, the local counterpart to an authenticated subject over the API.
const actorCLI = "cli"

const (
	pauseUsage  = "usage: cairnos pause <workflow> [--reason R] [--dir D]\n"
	resumeUsage = "usage: cairnos resume <workflow> [--dir D]\n"
	budgetUsage = "usage: cairnos budget <workflow> [--dir D]\n"
	auditUsage  = "usage: cairnos audit [--limit N] [--dir D]\n"
)

// cmdPause sets the persisted pause flag for a workflow so the engine refuses
// new runs and the scheduler skips firing it. The action is audit-logged.
func (a *App) cmdPause(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("pause", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	reasonFlag := flags.String("reason", "", "why the workflow is paused, recorded in the audit log")
	dirFlag := dirFlagOn(flags)

	name, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if name == "" {
		a.printErrf("%s", pauseUsage)

		return ExitUsage
	}

	db, _, code, proceed := openAt(ctx, a, "pause", *dirFlag, runStoreBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	if err := db.SetWorkflowPause(ctx, &store.WorkflowPause{WorkflowName: name, Actor: actorCLI, Reason: *reasonFlag}); err != nil {
		a.printErrf("pause: %v\n", err)

		return ExitFailure
	}

	a.recordAudit(ctx, db, store.AuditActionPause, name, *reasonFlag)
	a.printOutf("paused %s\n", name)

	return ExitOK
}

// cmdResume clears a workflow's pause flag. A workflow that was not paused is
// a reported no-op, not an error. The action is audit-logged.
func (a *App) cmdResume(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("resume", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	name, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if name == "" {
		a.printErrf("%s", resumeUsage)

		return ExitUsage
	}

	db, _, code, proceed := openAt(ctx, a, "resume", *dirFlag, runStoreBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	resumed, err := db.ClearWorkflowPause(ctx, name)
	if err != nil {
		a.printErrf("resume: %v\n", err)

		return ExitFailure
	}

	a.recordAudit(ctx, db, store.AuditActionResume, name, "")

	if resumed {
		a.printOutf("resumed %s\n", name)

		return ExitOK
	}

	a.printOutf("%s was not paused\n", name)

	return ExitOK
}

// cmdAudit lists governance audit rows newest-first.
func (a *App) cmdAudit(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("audit", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	limitFlag := flags.Int("limit", defaultListLimit, "maximum rows")
	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	if flags.NArg() > 0 {
		a.printErrf("%s", auditUsage)

		return ExitUsage
	}

	db, _, code, proceed := openAt(ctx, a, "audit", *dirFlag, runStoreBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	entries, err := db.ListAudit(ctx, store.AuditFilter{Limit: *limitFlag})
	if err != nil {
		a.printErrf("audit: %v\n", err)

		return ExitFailure
	}

	tab := newTab(a.stdout)

	a.tabWritef(tab, "WHEN\tACTOR\tACTION\tTARGET\tDETAIL\n")

	for _, entry := range entries {
		a.tabWritef(tab, "%s\t%s\t%s\t%s\t%s\n",
			formatTableTime(entry.CreatedAt), entry.Actor, entry.Action, entry.Target, entry.Detail)
	}

	a.flushTab(tab)

	return ExitOK
}

// cmdBudget shows a workflow's current-period spend against its limit plus its
// pause state. The budget spec comes from the workflow file under the home's
// workflows directory; spend comes from the store over the same period window
// the engine enforces against.
func (a *App) cmdBudget(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("budget", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	name, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if name == "" {
		a.printErrf("%s", budgetUsage)

		return ExitUsage
	}

	dir, err := resolveDir(*dirFlag, a.envLookup())
	if err != nil {
		a.printErrf("budget: %v\n", err)

		return ExitFailure
	}

	db, _, code, proceed := openAt(ctx, a, "budget", *dirFlag, runStoreBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	return a.printBudget(ctx, db, dir, name)
}

// printBudget renders one workflow's pause state and, when the file declares a
// budget, its spend-versus-limit line.
func (a *App) printBudget(ctx context.Context, db store.Store, dir, name string) int {
	_, paused, err := db.GetWorkflowPause(ctx, name)
	if err != nil {
		a.printErrf("budget: %v\n", err)

		return ExitFailure
	}

	a.printOutf("Workflow: %s\n", name)
	a.printOutf("Paused:   %t\n", paused)

	flow, loadErr := workflow.Load(filepath.Join(dir, workflowsDirName, name+workflowFileSuffix))
	if loadErr != nil || flow.Budget == nil {
		a.printOutf("Budget:   none\n")

		return ExitOK
	}

	since := engine.PeriodStart(flow.Budget.Period, time.Now())

	spend, err := db.SpendSince(ctx, name, since)
	if err != nil {
		a.printErrf("budget: %v\n", err)

		return ExitFailure
	}

	a.printOutf("Period:   %s (since %s)\n", flow.Budget.Period, formatTableTime(since))
	a.printOutf("Spend:    $%.4f of $%.4f\n", spend, flow.Budget.LimitUSD)
	a.printOutf("OnExceed: %s\n", flow.Budget.OnExceed)

	return ExitOK
}

// recordAudit appends a governance audit row from the CLI, logging a write
// failure rather than failing the command: the control action already
// happened.
func (a *App) recordAudit(ctx context.Context, db store.GovernanceStore, action, target, detail string) {
	entry := &store.AuditEntry{Actor: actorCLI, Action: action, Target: target, Detail: detail}
	if err := db.AppendAudit(ctx, entry); err != nil {
		a.logger.Warn("append governance audit", "action", action, "target", target, "error", err)
	}
}

// workflowFileSuffix selects workflow definitions in the workflows directory,
// mirroring the API resolver's suffix.
const workflowFileSuffix = ".toml"
