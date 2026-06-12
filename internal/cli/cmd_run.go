package cli

import (
	"context"
	"errors"
	"flag"

	"github.com/chadit/CairnOS/internal/agentloop"
	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/telemetry"
	"github.com/chadit/CairnOS/internal/workflow"
)

const runUsage = "usage: cairnos run <workflow.toml> [--input k=v ...] [--dir D]\n"

func (a *App) cmdRun(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	var inputs inputFlags

	flags.Var(&inputs, "input", "workflow input as key=value (repeatable)")

	dirFlag := dirFlagOn(flags)

	path, code, proceed := parsePositional(flags, args)
	if !proceed {
		return code
	}

	if path == "" {
		a.printErrf("%s", runUsage)

		return ExitUsage
	}

	flow, err := workflow.Load(path)
	if err != nil {
		a.printWorkflowErrors(err)

		return ExitUsage
	}

	values, err := CoerceInputs(flow.Inputs, inputs)
	if err != nil {
		a.printErrf("run: %v\n", err)

		return ExitUsage
	}

	db, cfg, code, proceed := a.openStoreAt(ctx, "run", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	mem, code, proceed := a.openMemoryAt(ctx, "run", *dirFlag)
	if !proceed {
		return code
	}
	defer a.closeStore(mem)

	return a.executeRun(ctx, &cfg, db, mem, flow, values)
}

// executeRun reconciles interrupted work, wires the engine (agent loop and
// telemetry included), runs the flow, and renders the outcome.
func (a *App) executeRun(ctx context.Context, cfg *config.Config, db store.Store, mem memory.Store, flow *workflow.Workflow, values map[string]any) int {
	reconciled, err := db.ReconcileInterrupted(ctx)
	if err != nil {
		a.printErrf("run: reconcile interrupted runs: %v\n", err)

		return ExitFailure
	}

	if reconciled > 0 {
		a.logger.Info("reconciled interrupted runs", "count", reconciled)
	}

	registry, err := newRegistry(ctx, cfg.Runners, mem, nil)
	if err != nil {
		a.printErrf("run: %v\n", err)

		return ExitFailure
	}
	defer a.closeRegistry(registry)

	// A one-shot run has nothing to scrape, and binding the configured
	// metrics port would collide with a serve process on the same machine,
	// so run never serves /metrics. The instrumentation hooks still fire:
	// with an OTLP endpoint configured, the run's trace and metrics push
	// to the collector on the deferred shutdown flush; without one, the
	// hooks cost nearly nothing and trace IDs still reach the logs.
	telCfg := cfg.Telemetry
	telCfg.Prometheus = ""

	tel, err := telemetry.Setup(ctx, telCfg, appVersion())
	if err != nil {
		a.printErrf("run: telemetry: %v\n", err)

		return ExitFailure
	}
	defer a.closeTelemetry(ctx, tel)

	engCfg := engineConfig(cfg)
	engCfg.Instrumentation = tel.Recorder()
	engCfg.AgentExecutor = agentloop.New(registry, mem, db, tel.Recorder())
	// A local run honors budgets and pauses too: governance is an engine
	// feature, not a serve-only one (.agents/spec/12-ui-and-governance.md).
	engCfg.Budget = db
	engCfg.Pause = db

	eng := engine.New(engCfg, registry, db)

	if err := wireBreakerTelemetry(ctx, eng, tel.Recorder()); err != nil {
		a.printErrf("run: telemetry: %v\n", err)

		return ExitFailure
	}

	run, err := eng.RunWorkflow(ctx, flow, values, triggerCLI)
	if err != nil {
		// Rejection before any record existed: input or registration
		// problems are validation-shaped, store failures are not.
		if _, isValidation := errors.AsType[cairnerr.Errors](err); isValidation {
			a.printWorkflowErrors(err)

			return ExitUsage
		}

		a.printErrf("run: %v\n", err)

		return ExitFailure
	}

	a.printRunSummary(ctx, db, run)

	if run.Status == store.RunSucceeded {
		return ExitOK
	}

	return ExitFailure
}

// printRunSummary renders per-action status and cost lines plus the run
// outcome.
func (a *App) printRunSummary(ctx context.Context, db store.Store, run *store.Run) {
	results, err := db.ListActionResults(ctx, run.ID)
	if err != nil {
		a.logger.Warn("list action results", "run", run.ID, "error", err)
	}

	a.writeActionTable(results)

	a.printOutf("\nrun %s (%s): %s\n", run.ID, run.WorkflowName, run.Status)

	if run.Error != nil {
		a.printOutf("error: %s: %s\n", run.Error.Code, run.Error.Message)
	}
}
