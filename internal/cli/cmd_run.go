package cli

import (
	"context"
	"errors"
	"flag"
	"path/filepath"

	"github.com/chadit/CairnOS/internal/agentloop"
	"github.com/chadit/CairnOS/internal/api"
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

	db, cfg, code, proceed := openAt(ctx, a, "run", *dirFlag, runStoreBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(db)

	mem, _, code, proceed := openAt(ctx, a, "run", *dirFlag, memoryBackends())
	if !proceed {
		return code
	}
	defer a.closeStore(mem)

	dir, err := resolveDir(*dirFlag, a.envLookup())
	if err != nil {
		a.printErrf("run: %v\n", err)

		return ExitFailure
	}

	return a.executeRun(ctx, &cfg, db, mem, flow, values, filepath.Join(dir, workflowsDirName))
}

// executeRun reconciles interrupted work, wires the engine (agent loop and
// telemetry included), runs the flow, and renders the outcome. A held run parked
// on an approval exits OK, because the operator decides it later with
// `cairnos runs approve`.
func (a *App) executeRun(ctx context.Context, cfg *config.Config, db store.Store, mem memory.Store, flow *workflow.Workflow, values map[string]any, workflowsDir string) int {
	reconciled, err := db.ReconcileInterrupted(ctx)
	if err != nil {
		a.printErrf("run: reconcile interrupted runs: %v\n", err)

		return ExitFailure
	}

	if reconciled > 0 {
		a.logger.Info("reconciled interrupted runs", "count", reconciled)
	}

	eng, cleanup, code, proceed := a.buildEngine(ctx, "run", workflowsDir, cfg, db, mem)
	if !proceed {
		return code
	}
	defer cleanup()

	run, err := eng.RunWorkflow(ctx, flow, values, triggerCLI)
	if err != nil {
		if rejectedBeforeAnyRecord(err) {
			a.printWorkflowErrors(err)

			return ExitUsage
		}

		a.printErrf("run: %v\n", err)

		return ExitFailure
	}

	a.printRunSummary(ctx, db, run)

	if run.Status == store.RunSucceeded || run.Status == store.RunHeld {
		return ExitOK
	}

	return ExitFailure
}

// buildEngine wires the runner registry, telemetry, agent loop, and governance
// onto an engine for a one-shot CLI command, returning a cleanup that flushes
// telemetry and closes the registry. run and replay share it, so both execute
// against an identically configured engine. A local command honors budgets and
// pauses too: governance is an engine feature, not a serve-only one.
func (a *App) buildEngine(ctx context.Context, label, workflowsDir string, cfg *config.Config, db store.Store, mem memory.Store) (*engine.Engine, func(), int, bool) {
	tel, err := telemetry.Setup(ctx, oneShotTelemetry(cfg.Telemetry), appVersion(), nil)
	if err != nil {
		a.printErrf("%s: telemetry: %v\n", label, err)

		return nil, nil, ExitFailure, false
	}

	source, closeSource, err := a.buildSecretSource(ctx, cairnHome(workflowsDir), cfg)
	if err != nil {
		a.closeTelemetry(ctx, tel)
		a.printErrf("%s: %v\n", label, err)

		return nil, nil, ExitFailure, false
	}

	credManager, err := buildCredentialManager(ctx, cfg, source)
	if err != nil {
		a.closeTelemetry(ctx, tel)
		a.runCloser("secret source", closeSource)
		a.printErrf("%s: %v\n", label, err)

		return nil, nil, ExitFailure, false
	}

	registry, err := newRegistry(ctx, cfg.Runners, sourcesFor(cfg.Sources), cfg.Hosts, a.envLookup(), mem, nil, credManager, nil,
		nil, a.logger, a.runnerTransport)
	if err != nil {
		a.closeTelemetry(ctx, tel)
		a.runCloser("secret source", closeSource)
		a.printErrf("%s: %v\n", label, err)

		return nil, nil, ExitFailure, false
	}

	engCfg := engineConfig(cfg, source)
	engCfg.Instrumentation = tel.Recorder()
	engCfg.AgentExecutor = agentloop.New(registry, mem, db, tel.Recorder())
	engCfg.Budget = db
	engCfg.Pause = db
	engCfg.Workflows = api.NewWorkflowResolver(workflowsDir, a.logger)

	eng := engine.New(engCfg, registry, db)

	cleanup := func() {
		a.closeTelemetry(ctx, tel)
		a.closeRegistry(registry)
		a.runCloser("secret source", closeSource)
	}

	if err := wireBreakerTelemetry(ctx, eng, tel.Recorder()); err != nil {
		cleanup()
		a.printErrf("%s: telemetry: %v\n", label, err)

		return nil, nil, ExitFailure, false
	}

	return eng, cleanup, ExitOK, true
}

// rejectedBeforeAnyRecord reports whether the engine refused before any run
// record existed. Input, registration, and governance problems are
// validation-shaped; store failures are not, and neither is a list naming no
// problem.
func rejectedBeforeAnyRecord(err error) bool {
	problems, isValidation := errors.AsType[cairnerr.Errors](err)

	return isValidation && len(problems) > 0
}

// oneShotTelemetry drops the Prometheus listener from a telemetry config. A
// one-shot command has nothing to scrape, and binding the port would collide
// with a serve process on the same machine. The hooks still fire: with an OTLP
// endpoint they push on the deferred shutdown flush, and without one they cost
// nearly nothing while trace IDs reach the logs.
func oneShotTelemetry(cfg config.Telemetry) config.Telemetry {
	cfg.Prometheus = ""

	return cfg
}

// cairnHome returns the home the secrets store resolves its path against, which
// is the parent of the workflows directory every caller passes.
func cairnHome(workflowsDir string) string {
	return filepath.Dir(workflowsDir)
}

// executeReplay wires the engine and replays a prior run, seeding its
// succeeded actions and re-executing the rest, then renders the outcome. The
// prior run, its results, and the current workflow arrive already resolved and
// hash-checked by the caller.
func (a *App) executeReplay(ctx context.Context, cfg *config.Config, db store.Store, mem memory.Store, prior *store.Run, results []*store.ActionResult, flow *workflow.Workflow, workflowsDir string) int {
	eng, cleanup, code, proceed := a.buildEngine(ctx, "runs replay", workflowsDir, cfg, db, mem)
	if !proceed {
		return code
	}
	defer cleanup()

	run, err := eng.ReplayRun(ctx, prior, results, flow)
	if err != nil {
		if rejectedBeforeAnyRecord(err) {
			a.printWorkflowErrors(err)

			return ExitUsage
		}

		a.printErrf("runs replay: %v\n", err)

		return ExitFailure
	}

	a.printOutf("replaying run %s as %s\n", prior.ID, run.ID)
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
