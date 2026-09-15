package cli

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/chadit/CairnOS/internal/agentloop"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/hostenv"
	"github.com/chadit/CairnOS/internal/poll"
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

	// A poll workflow naming an unregistered source parses fine but fails at
	// every fire (poll.ErrUnknownSource is a runtime lookup, not a load-time
	// rule), so warn now while the operator is watching rather than only in the
	// daemon log later.
	if flow.Poll != nil && !slices.Contains(poll.BuiltinSourceNames(), flow.Poll.Source) {
		a.printErrf("schedule add: warning: unknown poll source %q; the schedule will fail at fire time\n", flow.Poll.Source)
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

	dir, err := resolveDir(*dirFlag)
	if err != nil {
		a.printErrf("schedule run: %v\n", err)

		return ExitFailure
	}

	// The source is built before the registry so the credential manager and the
	// runners share one secret chain.
	source, closeSource, err := a.buildSecretSource(ctx, dir, &cfg)
	if err != nil {
		a.printErrf("schedule run: %v\n", err)

		return ExitFailure
	}
	defer func() { _ = closeSource() }()

	// Outbound credentials resolve their secret refs through the source so the
	// http and mcp_client runners mint bearer tokens. Construction does no
	// network and fails fast on a missing ref.
	credManager, err := buildCredentialManager(ctx, &cfg, source)
	if err != nil {
		a.printErrf("schedule run: %v\n", err)

		return ExitFailure
	}

	registry, err := newRegistry(ctx, cfg.Runners, sourceSpecs(cfg.Sources), hostenv.Render(cfg.Hosts), mem, nil, credManager, nil)
	if err != nil {
		a.printErrf("schedule run: %v\n", err)

		return ExitFailure
	}
	defer a.closeRegistry(registry)

	engCfg := engineConfig(&cfg, source)
	engCfg.AgentExecutor = agentloop.New(registry, mem, db, nil)

	fireRunner := &engineFireRunner{
		eng: engine.New(engCfg, registry, db),
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

	// The `schedule run` worker fans held and dead_lettered alerts the same way
	// serve does, so an operator running the scheduler outside serve still gets
	// Discord notifications. It holds no messaging identities, so the nil registry
	// drops the Mattermost sink. The drainer rides a child context so it stops
	// once the producer and worker return, even on an early error.
	fanoutCtx, stopFanout := context.WithCancel(ctx)
	fanout := BuildFanout(&cfg, nil, a.logger)
	fanoutDone := StartFanout(fanoutCtx, fanout)

	// The daemon enqueues each fire; the worker drains the queue and runs the
	// workflow through the engine fire runner. A fire no worker can take right
	// away waits in the queue instead of being skipped (S8). The poll-aware
	// wrapper turns a [workflow.poll] fire into per-event enqueues and skips a
	// workflow whose required host the reach monitor reports down.
	monitor := newReachMonitor(fanout, a.logger)
	enqueue := &scheduleEnqueueRunner{queue: queue, logger: a.logger}
	pollRunner := newPollAwareRunner(enqueue, queue, buildPollSources(registry), db, monitor, a.logger)

	daemon := scheduler.New(scheduler.Config{
		Store:       scheds,
		Runner:      pollRunner,
		Coordinator: coord,
		Logger:      a.logger,
	})

	worker := newQueueWorker(queue, &QueueRunnerAdapter{fire: fireRunner, reach: monitor}, &cfg.Scheduler, nil, fanout, monitor, a.logger)

	a.printOutf("scheduler running; interrupt to stop\n")

	go monitor.Run(fanoutCtx)

	runErr := runProducerAndWorker(ctx, daemon, worker)

	stopFanout()
	<-fanoutDone

	if runErr != nil {
		a.printErrf("schedule run: %v\n", runErr)

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

// Fire loads and runs one scheduled workflow with default inputs and the
// schedule trigger. It is FireRun with nothing extra, so any holder of a
// scheduler.Runner sees no change.
func (r *engineFireRunner) Fire(ctx context.Context, sched *scheduler.Schedule) error {
	return r.FireRun(ctx, sched, nil, triggerSchedule)
}

// FireRun loads and runs one scheduled workflow with caller-supplied inputs and
// a trigger label, the poll fire path's entry point on the schedule-run daemon.
// A failed run surfaces as an error so the daemon logs it; the daemon keeps
// running either way.
func (r *engineFireRunner) FireRun(ctx context.Context, sched *scheduler.Schedule, inputs map[string]any, trigger string) error {
	flow, err := workflow.Load(sched.WorkflowPath)
	if err != nil {
		return fmt.Errorf("load %s: %w", sched.WorkflowPath, err)
	}

	if inputs == nil {
		inputs = map[string]any{}
	}

	run, err := r.eng.RunWorkflow(ctx, flow, inputs, trigger)
	if err != nil {
		return fmt.Errorf("run %s: %w", sched.WorkflowName, err)
	}

	if run.Status != store.RunSucceeded {
		return fmt.Errorf("%w: run %s finished %s", ErrScheduledRunFailed, run.ID, run.Status)
	}

	return nil
}
