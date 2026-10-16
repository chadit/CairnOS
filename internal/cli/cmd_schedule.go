package cli

import (
	"context"
	"flag"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/chadit/CairnOS/internal/agentloop"
	"github.com/chadit/CairnOS/internal/engine"
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
	case subcmdRemove:
		return a.cmdScheduleRemove(ctx, args[1:])
	case subcmdRun:
		return a.cmdScheduleRun(ctx, args[1:])
	default:
		a.printErrf("unknown schedule subcommand %q\n", args[0])
		a.printErrf("%s", scheduleUsage)

		return ExitUsage
	}
}

// cmdScheduleAdd stores a workflow file's schedule key
// (.agents/spec/02-workflow-schema.md) as an absolute path, so the daemon can
// load it from any directory. A required input no fire supplies and an
// unregistered poll source both parse fine and fail every fire, so add refuses
// the first and warns on the second while the operator is watching.
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

	if flow.Schedule == "" {
		a.printErrf("schedule add: %s has no schedule key; add one (e.g. schedule = \"@hourly\")\n", path)

		return ExitUsage
	}

	if missing := requiredInputsWithoutDefault(flow); len(missing) > 0 {
		a.printErrf("schedule add: %s: required input(s) with no default that no scheduled fire supplies: %s; give each one a default\n",
			path, strings.Join(missing, ", "))

		return ExitUsage
	}

	if flow.Poll != nil {
		if _, known := poll.BuiltinSourceInputs(flow.Poll.Source); !known {
			a.printErrf("schedule add: warning: unknown poll source %q; the schedule will fail at fire time\n", flow.Poll.Source)
		}
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		a.printErrf("schedule add: resolve path: %v\n", err)

		return ExitFailure
	}

	scheds, _, code, proceed := openAt(ctx, a, "schedule add", *dirFlag, scheduleBackends())
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

	scheds, _, code, proceed := openAt(ctx, a, "schedule list", *dirFlag, scheduleBackends())
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

	scheds, _, code, proceed := openAt(ctx, a, "schedule remove", *dirFlag, scheduleBackends())
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

// reconcileInterrupted clears runs a previous process left mid-flight and
// reports whether the daemon can carry on. It runs first because the daemon may
// be the first thing started after a crash.
func (a *App) reconcileInterrupted(ctx context.Context, label string, db store.Store) bool {
	reconciled, err := db.ReconcileInterrupted(ctx)
	if err != nil {
		a.printErrf("%s: reconcile interrupted runs: %v\n", label, err)

		return false
	}

	if reconciled > 0 {
		a.logger.Info("reconciled interrupted runs", "count", reconciled)
	}

	return true
}

// cmdScheduleRun is the foreground Phase 0b daemon: the daemon enqueues each
// due fire and the worker drains the queue through the engine fire runner, until
// the context is canceled, which exits clean. Phase 1a's serve embeds the same
// loop.
//
// It fans held and dead_lettered alerts the way serve does, so running the
// scheduler alone still notifies.
func (a *App) cmdScheduleRun(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("schedule run", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	db, cfg, code, proceed := openAt(ctx, a, "schedule run", *dirFlag, runStoreBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	scheds, _, code, proceed := openAt(ctx, a, "schedule run", *dirFlag, scheduleBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(scheds)

	mem, _, code, proceed := openAt(ctx, a, "schedule run", *dirFlag, memoryBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(mem)

	if !a.reconcileInterrupted(ctx, "schedule run", db) {
		return ExitFailure
	}

	dir, err := resolveDir(*dirFlag, a.envLookup())
	if err != nil {
		a.printErrf("schedule run: %v\n", err)

		return ExitFailure
	}

	source, closeSource, err := a.buildSecretSource(ctx, dir, &cfg)
	if err != nil {
		a.printErrf("schedule run: %v\n", err)

		return ExitFailure
	}
	defer a.runCloser("secret source", closeSource)

	credManager, err := buildCredentialManager(ctx, &cfg, source)
	if err != nil {
		a.printErrf("schedule run: %v\n", err)

		return ExitFailure
	}

	registry, err := newRegistry(ctx, cfg.Runners, sourceSpecs(cfg.Sources), cfg.Hosts, a.envLookup(), mem, nil, credManager, nil,
		nil, a.logger, nil)
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

	queue, err := workQueueBackends(scheds).open(ctx, dir, &cfg)
	if err != nil {
		a.printErrf("schedule run: %v\n", err)

		return ExitFailure
	}
	defer a.runCloser("work queue", ownedCloser(queue))

	fanoutCtx, stopFanout := context.WithCancel(ctx)
	fanout := BuildFanout(&cfg, nil, a.envLookup(), a.logger, nil)
	fanoutDone := StartFanout(fanoutCtx, fanout)

	monitor := newReachMonitor(fanout, a.logger)
	enqueue := scheduler.NewSkipOverlapping(&scheduleEnqueueRunner{queue: queue, logger: a.logger}, queue, a.logger)
	pollRunner := newPollAwareRunner(enqueue, queue, buildPollSources(registry), db, monitor, a.logger)

	daemonCfg := scheduler.Config{Store: scheds, Runner: pollRunner, Logger: a.logger}
	if coord := newCoordinator(scheds); coord != nil {
		daemonCfg.Coordinator = coord
		defer closeCoordinator(ctx, coord, a.logger)
	}

	daemon := scheduler.New(daemonCfg)

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

// requiredInputsWithoutDefault lists, sorted, the required inputs with no
// default that a fire of flow never supplies, so the engine rejects every run.
// A plain scheduled fire supplies no inputs, and a poll fire only the keys its
// source emits. A poll source no wiring builds is skipped, since its keys are
// unknown; add warns about it instead.
func requiredInputsWithoutDefault(flow *workflow.Workflow) []string {
	var supplied []string

	if flow.Poll != nil {
		emitted, known := poll.BuiltinSourceInputs(flow.Poll.Source)
		if !known {
			return nil
		}

		supplied = emitted
	}

	var missing []string

	for _, name := range slices.Sorted(maps.Keys(flow.Inputs)) {
		if flow.Inputs[name].MustBeSupplied() && !slices.Contains(supplied, name) {
			missing = append(missing, name)
		}
	}

	return missing
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
