package cli

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/chadit/CairnOS/internal/agentloop"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/scheduler"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
)

const scheduleUsage = "usage: cairnos schedule <add|list|remove|run> [flags]\n"

// triggerSchedule marks runs fired by the scheduler daemon in run history,
// distinguishing them from operator-started cli runs.
const triggerSchedule = "schedule"

func (a *App) cmdSchedule(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printErrf("%s", scheduleUsage)

		return ExitUsage
	}

	switch args[0] {
	case subcmdAdd:
		return a.cmdScheduleAdd(ctx, args[1:])
	case subcmdList:
		return a.cmdScheduleList(ctx, args[1:])
	case "remove":
		return a.cmdScheduleRemove(ctx, args[1:])
	case "run":
		return a.cmdScheduleRun(ctx, args[1:])
	default:
		a.printErrf("unknown schedule subcommand %q\n", args[0])
		a.printErrf("%s", scheduleUsage)

		return ExitUsage
	}
}

func (a *App) cmdScheduleAdd(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("schedule add", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	path, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if path == "" {
		a.printErrf("usage: cairnos schedule add <workflow.toml> [--dir D]\n")

		return ExitUsage
	}

	flow, err := workflow.Load(path)
	if err != nil {
		a.printWorkflowErrors(err)

		return ExitUsage
	}

	// The cron expression lives in the workflow file's schedule key
	// (.agents/spec/02-workflow-schema.md); a file without one has nothing
	// to schedule.
	if flow.Schedule == "" {
		a.printErrf("schedule add: %s has no schedule key; add one (e.g. schedule = \"@hourly\")\n", path)

		return ExitUsage
	}

	// Store the absolute path so the daemon can load the file no matter
	// which directory it starts from.
	absPath, err := filepath.Abs(path)
	if err != nil {
		a.printErrf("schedule add: resolve path: %v\n", err)

		return ExitFailure
	}

	scheds, code, proceed := a.openSchedulesAt(ctx, "schedule add", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(scheds)

	id, err := scheds.Add(ctx, &scheduler.Schedule{
		WorkflowPath: absPath,
		WorkflowName: flow.Name,
		Cron:         flow.Schedule,
	})
	if err != nil {
		a.printErrf("schedule add: %v\n", err)

		return ExitFailure
	}

	a.printOutf("%s\n", id)

	return ExitOK
}

func (a *App) cmdScheduleList(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("schedule list", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	scheds, code, proceed := a.openSchedulesAt(ctx, "schedule list", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(scheds)

	list, err := scheds.List(ctx)
	if err != nil {
		a.printErrf("schedule list: %v\n", err)

		return ExitFailure
	}

	tab := newTab(a.stdout)

	a.tabWritef(tab, "ID\tNAME\tCRON\tLAST FIRED\tNEXT FIRE\n")

	for idx := range list {
		sched := &list[idx]

		a.tabWritef(tab, "%s\t%s\t%s\t%s\t%s\n",
			sched.ID, sched.WorkflowName, sched.Cron,
			formatTableTime(sched.LastFired), formatTableTime(sched.NextFire))
	}

	a.flushTab(tab)

	return ExitOK
}

func (a *App) cmdScheduleRemove(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("schedule remove", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	key, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if key == "" {
		a.printErrf("usage: cairnos schedule remove <id-or-name> [--dir D]\n")

		return ExitUsage
	}

	scheds, code, proceed := a.openSchedulesAt(ctx, "schedule remove", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(scheds)

	if err := scheds.Remove(ctx, key); err != nil {
		a.printErrf("schedule remove: %v\n", err)

		return ExitFailure
	}

	a.printOutf("removed %s\n", key)

	return ExitOK
}

// cmdScheduleRun is the foreground Phase 0b daemon: it fires due schedules
// through the engine until the context is canceled, which exits clean.
// Phase 1a's serve embeds the same loop.
func (a *App) cmdScheduleRun(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("schedule run", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	db, cfg, code, proceed := a.openStoreAt(ctx, "schedule run", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	scheds, code, proceed := a.openSchedulesAt(ctx, "schedule run", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(scheds)

	mem, code, proceed := a.openMemoryAt(ctx, "schedule run", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(mem)

	// Reconcile work a previous process left mid-flight, same as cmdRun:
	// the daemon may be the first thing started after a crash.
	reconciled, err := db.ReconcileInterrupted(ctx)
	if err != nil {
		a.printErrf("schedule run: reconcile interrupted runs: %v\n", err)

		return ExitFailure
	}

	if reconciled > 0 {
		a.logger.Info("reconciled interrupted runs", "count", reconciled)
	}

	registry, err := newRegistry(ctx, cfg.Runners, mem, nil)
	if err != nil {
		a.printErrf("schedule run: %v\n", err)

		return ExitFailure
	}
	defer a.closeRegistry(registry)

	engCfg := engineConfig(&cfg)
	engCfg.AgentExecutor = agentloop.New(registry, mem, db, nil)

	fireRunner := &engineFireRunner{
		eng: engine.New(engCfg, registry, db),
	}

	dir, err := resolveDir(*dirFlag)
	if err != nil {
		a.printErrf("schedule run: %v\n", err)

		return ExitFailure
	}

	queue, closeQueue, err := openWorkQueue(ctx, dir, &cfg, scheds)
	if err != nil {
		a.printErrf("schedule run: %v\n", err)

		return ExitFailure
	}
	defer func() { _ = closeQueue() }()

	// On a Postgres backend this is the advisory-lock coordinator, so two
	// `cairnos schedule run` nodes against one database never double-enqueue; on
	// sqlite it is the single-node SoloCoordinator
	// (.agents/spec/14-open-decisions.md, S2).
	coord, closeCoord := newCoordinator(scheds)
	if closeCoord != nil {
		defer closeCoord(context.WithoutCancel(ctx))
	}

	// The daemon enqueues each fire; the worker drains the queue and runs the
	// workflow through the engine fire runner. A fire no worker can take right
	// away waits in the queue instead of being skipped (S8).
	daemon := scheduler.New(scheduler.Config{
		Store:       scheds,
		Runner:      &scheduleEnqueueRunner{queue: queue, logger: a.logger},
		Coordinator: coord,
		Logger:      a.logger,
	})

	worker := newQueueWorker(queue, &queueRunnerAdapter{fire: fireRunner}, &cfg.Scheduler, nil, a.logger)

	a.printOutf("scheduler running; interrupt to stop\n")

	if err := runProducerAndWorker(ctx, daemon, worker); err != nil {
		a.printErrf("schedule run: %v\n", err)

		return ExitFailure
	}

	return ExitOK
}

// engineFireRunner adapts the engine to the scheduler's Runner: each fire
// loads the workflow file fresh (so edits apply without re-adding the
// schedule) and runs it with default inputs only.
type engineFireRunner struct {
	eng *engine.Engine
}

// Fire loads and runs one scheduled workflow. A failed run surfaces as an
// error so the daemon logs it; the daemon keeps running either way.
func (r *engineFireRunner) Fire(ctx context.Context, sched *scheduler.Schedule) error {
	flow, err := workflow.Load(sched.WorkflowPath)
	if err != nil {
		return fmt.Errorf("load %s: %w", sched.WorkflowPath, err)
	}

	run, err := r.eng.RunWorkflow(ctx, flow, map[string]any{}, triggerSchedule)
	if err != nil {
		return fmt.Errorf("run %s: %w", sched.WorkflowName, err)
	}

	if run.Status != store.RunSucceeded {
		return fmt.Errorf("%w: run %s finished %s", ErrScheduledRunFailed, run.ID, run.Status)
	}

	return nil
}
