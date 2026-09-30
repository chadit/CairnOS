package cli

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/messaging"
	"github.com/chadit/CairnOS/internal/reach"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/scheduler"
	"github.com/chadit/CairnOS/pkg/runner"
)

// HomeEnvName is the variable the commands resolve a home directory from. It
// differs per platform the way os.UserHomeDir does, so a test stages this key
// rather than hard-coding HOME.
func HomeEnvName() string {
	return homeEnvKey()
}

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

// ErrNoSourceCheckout is the refusal a directory carrying no go.mod or no .git
// gets from UpdateSourceDir, exported so a case names the sentinel rather than
// matching the message text.
var ErrNoSourceCheckout = errNoSourceCheckout

// ErrUpdateBusy is the refusal an update gets while another holds the lock,
// exported so a case names the sentinel rather than matching the message text.
var ErrUpdateBusy = errUpdateBusy

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

// UpdateBuild runs the update's build step, so a test can stage stub make and
// go executables through WithEnv and see the environment make is started with.
func (a *App) UpdateBuild(ctx context.Context, dir string) error {
	return a.buildBinary(ctx, dir)
}

// UpdateLink runs the update's link step against a given directory, so a test
// uses a temporary one rather than the invoking user's ~/.local/bin.
func UpdateLink(binary, linkDir string) error {
	return ensureLink(binary, linkDir)
}

// RunUpdateSteps drives the update's step sequence against substitutes, which
// is how a test pins the pull-before-restart order without a vault or a unit.
// The revision reads around the steps stay real, so dir must be a checkout.
func (*App) RunUpdateSteps(ctx context.Context, dir, home string, steps UpdateSteps) error {
	return applyUpdate(ctx, dir, &updateOptions{home: home, link: true}, steps)
}

// UpdateRun names one invocation's inputs for a test: the release API to ask,
// the binary the install replaces, and the flags the real command parses. The
// two resolved fields are why this exists, since a test must never reach
// api.github.com or overwrite the test binary.
type UpdateRun struct {
	// Transport carries the release lookup and the asset download. A test
	// passes one that answers from a handler in process, so the case needs no
	// listening socket and cannot be failed by the host's network stack.
	Transport http.RoundTripper

	APIBase    string
	Executable string
	Home       string
	Source     string

	// Running is the revision the case says is running, where the command reads
	// it out of the service's process image and serve's loop out of its own.
	// Empty is nothing running, so a case never depends on the host's service.
	Running string

	Check     bool
	NoFetch   bool
	NoRelease bool
}

// options resolves the run the way cmdUpdate resolves its flags.
func (run *UpdateRun) options() *updateOptions {
	return &updateOptions{
		source:     run.Source,
		home:       run.Home,
		executable: run.Executable,
		apiBase:    run.APIBase,
		transport:  run.Transport,
		running:    run.Running,
		check:      run.Check,
		fetch:      !run.NoFetch,
		link:       true,
		release:    !run.NoRelease,
	}
}

// DriveUpdate runs the whole release-or-source decision against staged inputs
// and substituted steps, and returns the exit code the command would.
func (a *App) DriveUpdate(ctx context.Context, run *UpdateRun, steps UpdateSteps) (int, error) {
	return a.runUpdate(ctx, run.options(), steps)
}

// AutoUpdateLoop is serve's self-update loop, held so a case can drive its
// ticks one at a time and see what it remembers between them.
type AutoUpdateLoop struct {
	loop *autoUpdate
}

// NewAutoUpdateLoop builds the loop over a staged invocation. run.Running
// stands in for the loop's own stamp, and steps replace every step but the
// drain, which stays the loop's own so a case sees whether it was asked for.
func (a *App) NewAutoUpdateLoop(run *UpdateRun, drain *api.Drain, steps UpdateSteps) *AutoUpdateLoop {
	return &AutoUpdateLoop{loop: a.newAutoUpdate(run.options(), drain, steps, a.logger)}
}

// Tick runs one check and reports whether the loop is finished, which it is
// once the process is draining for the new build.
func (l *AutoUpdateLoop) Tick(ctx context.Context) bool {
	return l.loop.tick(ctx)
}

// RunAutoUpdateStart runs serve's decision to start the loop against the
// supervisor answer a case names, empty meaning supervised, so no case asks the
// host's systemctl. It returns the channel serve waits on, nil when the loop
// did not start; a started loop runs until ctx ends.
func (a *App) RunAutoUpdateStart(ctx context.Context, update *config.Update, home, unsupervised string) <-chan struct{} {
	a.unsupervised = func(context.Context) string { return unsupervised }

	return a.startAutoUpdate(ctx, update, home, api.NewDrain(), a.logger)
}

// HoldUpdateLock takes the update lock under home the way a running update
// does, so a case can watch the other side refuse or wait. The returned func
// lets it go.
func HoldUpdateLock(home string) (func(), error) {
	return lockUpdate(home)
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

// NewMessageSender exposes serve's send adapter, so a test can drive the message
// runner through the same error translation serve applies.
func NewMessageSender(registry *messaging.Registry, logger *slog.Logger) builtins.MessageSender {
	return &messageSenderAdapter{registry: registry, logger: logger}
}

// ServeChatReadTools exposes the read tools serve hands the chat agent when only
// the runner registry is present, so a case can pin which runners chat reuses.
func ServeChatReadTools(registry *runner.Registry) map[string]runner.Runner {
	return chatReadTools(nil, nil, nil, registry)
}
