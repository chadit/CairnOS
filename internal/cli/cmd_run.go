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
	"github.com/chadit/CairnOS/internal/hostenv"
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

	dir, err := resolveDir(*dirFlag)
	if err != nil {
		a.printErrf("run: %v\n", err)

		return ExitFailure
	}

	return a.executeRun(ctx, &cfg, db, mem, flow, values, filepath.Join(dir, workflowsDirName))
}

// executeRun reconciles interrupted work, wires the engine (agent loop and
// telemetry included), runs the flow, and renders the outcome.
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

	// A held run parked on an approval is a clean outcome, not a failure: the
	// operator decides it later with `cairnos runs approve`
	// (.agents/spec/parity/workflow-language.md, "Human approval").
	if run.Status == store.RunSucceeded || run.Status == store.RunHeld {
		return ExitOK
	}

	return ExitFailure
}

// buildEngine wires the runner registry, telemetry, agent loop, and
// governance onto an engine for a one-shot CLI command, returning a cleanup
// that flushes telemetry and closes the registry. run and replay share it so
// both execute against an identically configured engine. A local command
// honors budgets and pauses too: governance is an engine feature, not a
// serve-only one (.agents/spec/12-ui-and-governance.md).
func (a *App) buildEngine(ctx context.Context, label, workflowsDir string, cfg *config.Config, db store.Store, mem memory.Store) (*engine.Engine, func(), int, bool) {
	// A one-shot command has nothing to scrape, and binding the metrics port
	// would collide with a serve process on the same machine, so it never
	// serves /metrics. The hooks still fire: with an OTLP endpoint they push on
	// the deferred shutdown flush, and without one they cost nearly nothing
	// while trace IDs reach the logs.
	telCfg := cfg.Telemetry
	telCfg.Prometheus = ""

	tel, err := telemetry.Setup(ctx, telCfg, appVersion())
	if err != nil {
		a.printErrf("%s: telemetry: %v\n", label, err)

		return nil, nil, ExitFailure, false
	}

	// The secrets store shares the run store's DSN; its path resolves against the
	// CairnOS home, the parent of the workflows directory every caller passes
	// (filepath.Join(dir, workflowsDirName)). The default env backend opens
	// nothing and hands back a no-op closer. The source is built before the
	// registry so the credential manager and the runners share one chain.
	source, closeSource, err := a.buildSecretSource(ctx, filepath.Dir(workflowsDir), cfg)
	if err != nil {
		a.closeTelemetry(ctx, tel)
		a.printErrf("%s: %v\n", label, err)

		return nil, nil, ExitFailure, false
	}

	// Outbound credentials resolve their secret refs through the same source, so
	// an http or mcp_client runner naming a credential mints a bearer token.
	// Construction does no network; a missing ref fails here, not mid-run.
	credManager, err := buildCredentialManager(ctx, cfg, source)
	if err != nil {
		a.closeTelemetry(ctx, tel)
		a.closeSecretSource(closeSource)
		a.printErrf("%s: %v\n", label, err)

		return nil, nil, ExitFailure, false
	}

	registry, err := newRegistry(ctx, cfg.Runners, sourceSpecs(cfg.Sources), hostenv.Render(cfg.Hosts), mem, nil, credManager, nil)
	if err != nil {
		a.closeTelemetry(ctx, tel)
		a.closeSecretSource(closeSource)
		a.printErrf("%s: %v\n", label, err)

		return nil, nil, ExitFailure, false
	}

	engCfg := engineConfig(cfg, source)
	engCfg.Instrumentation = tel.Recorder()
	engCfg.AgentExecutor = agentloop.New(registry, mem, db, tel.Recorder())
	engCfg.Budget = db
	engCfg.Pause = db
	// A workflow-call action resolves its child by name from the workflows
	// directory, the same directory-backed resolver the API and replay use
	// (.agents/spec/parity/workflow-language.md, "Workflow call").
	engCfg.Workflows = api.NewWorkflowResolver(workflowsDir, a.logger)

	eng := engine.New(engCfg, registry, db)

	cleanup := func() {
		a.closeTelemetry(ctx, tel)
		a.closeRegistry(registry)

		_ = closeSource()
	}

	if err := wireBreakerTelemetry(ctx, eng, tel.Recorder()); err != nil {
		cleanup()
		a.printErrf("%s: telemetry: %v\n", label, err)

		return nil, nil, ExitFailure, false
	}

	return eng, cleanup, ExitOK, true
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
		// Rejection before any record existed: registration or governance
		// problems are validation-shaped, store failures are not.
		if _, isValidation := errors.AsType[cairnerr.Errors](err); isValidation {
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
