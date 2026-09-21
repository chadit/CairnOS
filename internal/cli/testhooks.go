package cli

import (
	"context"
	"log/slog"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/reach"
	"github.com/chadit/CairnOS/internal/scheduler"
)

// ConfigFileName is the config file name WriteConfigBlock edits.
const ConfigFileName = configFileName

// LocalRunnerHeader is the TOML section header a model setup writes.
const LocalRunnerHeader = localRunnerHeader

// FlagPort, FlagThreads, and FlagCtxSize are the model subcommand's numeric
// flag names.
const (
	FlagPort    = flagPort
	FlagThreads = flagThreads
	FlagCtxSize = flagCtxSize
)

// This file exists so internal/cli's _test.go files can live in the external
// cli_test package (cairnlint's externaltestpkg rule) while still driving
// ModelOptions/ModelTarget resolution and the reach-gated queue wiring, which
// have no other public entry point that stays a fast, offline unit test (the
// real CLI path downloads a live runtime and model). It is not itself a
// _test.go file, so it compiles into the package like
// internal/memory/testhelpers.go and its siblings already do elsewhere in
// this repo.

// NewModelOptions builds a ModelOptions with the fields
// TestManualStartHintIsRunnable exercises; the rest stay zero-valued.
func NewModelOptions(dir, model string, port, threads, ctxSize int) *ModelOptions {
	return &ModelOptions{dir: dir, model: model, port: port, threads: threads, ctxSize: ctxSize}
}

// ServerPath returns the resolved target's server binary path.
func (t *ModelTarget) ServerPath() string {
	return t.layout.ServerPath()
}

// ModelPath returns the resolved target's model weights path.
func (t *ModelTarget) ModelPath() string {
	return t.layout.ModelPath(&t.model)
}

// NewDirectPollAwareRunner builds a PollAwareRunner directly from its dependencies,
// bypassing newPollAwareRunner's queue-capability probe, so a reach-gate test
// can drive it against a fake queue and monitor.
func NewDirectPollAwareRunner(inner scheduler.Runner, queue pollQueue, monitor *reach.Monitor, logger *slog.Logger) *PollAwareRunner {
	return &PollAwareRunner{inner: inner, queue: queue, reach: monitor, logger: logger}
}

// Reach returns the reach monitor a PollAwareRunner gates fires on.
func (r *PollAwareRunner) Reach() *reach.Monitor {
	return r.reach
}

// NewQueueRunnerAdapter builds a QueueRunnerAdapter directly, for a
// reach-gate test that drives Run against a fake inner runner.
func NewQueueRunnerAdapter(fire inputsRunner, monitor *reach.Monitor) *QueueRunnerAdapter {
	return &QueueRunnerAdapter{fire: fire, reach: monitor}
}

// UpdateSourceDir resolves the checkout `cairnos update` builds from, given a
// --source override and the path of the running binary. A test passes a staged
// layout where the real command passes its own executable.
func UpdateSourceDir(override, executable string) (string, error) {
	return discoverSource(override, executable)
}

// UpdateFastForward runs the update's fetch-and-fast-forward step against a
// real checkout, which is the only way to exercise the decision honestly.
func (a *App) UpdateFastForward(ctx context.Context, dir string) error {
	return a.syncCheckout(ctx, dir)
}

// UpdateLink runs the update's link step against a given directory, so a test
// uses a temporary one rather than the invoking user's ~/.local/bin.
func UpdateLink(binary, linkDir string) error {
	return ensureLink(binary, linkDir)
}

// RunUpdateSteps drives the update's step sequence against substitutes, which
// is how a test pins the pull-before-restart order without a vault or a unit.
// The revision reads around the steps stay real, so dir must be a checkout.
func (a *App) RunUpdateSteps(ctx context.Context, dir, home string, steps UpdateSteps) error {
	return a.applyUpdate(ctx, dir, updateOptions{home: home, link: true}, steps)
}

// UpdateRun names one invocation's inputs for a test: the release API to ask,
// the binary the install replaces, and the flags the real command parses. The
// two resolved fields are why this exists, since a test must never reach
// api.github.com or overwrite the test binary.
type UpdateRun struct {
	APIBase    string
	Executable string
	Home       string
	Source     string
	Check      bool
	NoFetch    bool
	NoRelease  bool
}

// DriveUpdate runs the whole release-or-source decision against staged inputs
// and substituted steps, and returns the exit code the command would.
func (a *App) DriveUpdate(ctx context.Context, run UpdateRun, steps UpdateSteps) (int, error) {
	return a.runUpdate(ctx, updateOptions{
		source:     run.Source,
		home:       run.Home,
		executable: run.Executable,
		apiBase:    run.APIBase,
		check:      run.Check,
		fetch:      !run.NoFetch,
		link:       true,
		release:    !run.NoRelease,
	}, steps)
}

// UpdateRevisionReport prints one revision pair and answers with the exit code
// the command would, so a case pins both outcomes without a running service to
// read a stamp out of.
func (a *App) UpdateRevisionReport(running, wanted string) int {
	return a.reportRevisionPair(running, wanted)
}

// NewDrainGateRunner wraps a scheduler runner in the gate serve puts in front
// of its daemon, so a case drives the skip without standing up a whole serve.
func NewDrainGateRunner(inner scheduler.Runner, drain *api.Drain, logger *slog.Logger) scheduler.Runner {
	return &drainGateRunner{inner: inner, drain: drain, logger: logger}
}

// RunDrainWatcher runs serve's drain watcher, which is what turns an idle
// process or a passed ceiling into the context cancel that ends the process.
func RunDrainWatcher(ctx context.Context, drain *api.Drain, inFlight func() int, stop context.CancelFunc, logger *slog.Logger) {
	watchDrain(ctx, drain, inFlight, stop, logger)
}

// UpdateRestartHooks is the pair `cairnos update` turns a service over through:
// the drain request to the running build, and the supervisor restart behind it.
type UpdateRestartHooks = restartHooks

// UpdateDriveRestart runs the restart choice against substituted hooks and
// reports whether the drain was accepted, so a case pins the preference, the
// fallback, and --restart-now without a supervisor or a running service.
func (a *App) UpdateDriveRestart(ctx context.Context, hooks UpdateRestartHooks, restartNow bool) (bool, error) {
	return a.driveRestart(ctx, hooks, restartNow)
}

// UpdateInstallBinary runs the in-place replacement on its own, so a case that
// substitutes the install step can still record the order and then perform the
// real swap on a file it owns.
func UpdateInstallBinary(target, staged string) error {
	return installBinary(target, staged)
}
