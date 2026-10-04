package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/service"
)

// autoUpdateTimeout bounds each half of a tick on its own: the lookup, then the
// install with its build and secrets pull. Past it the build's whole process
// group is signaled, so a fetch or build that hangs cannot hold the loop, and
// every tick after it, for as long as serve runs.
const autoUpdateTimeout = 30 * time.Minute

// unstampedRevision stands in for a running build with no vcs stamp in the line
// that starts an update, where an empty field would read as a logging bug.
const unstampedRevision = "unstamped"

// autoUpdateAttemptName is the file under the CairnOS home that holds the
// revision the loop last tried to install, and attemptFilePerm its mode.
const (
	autoUpdateAttemptName = "auto-update-attempt"
	attemptFilePerm       = 0o600
)

// The stages a failed auto-update names, so the error line says what broke
// without the reader digging it out of the message.
const (
	stepDownload = "download"
	stepInstall  = "install"
	stepCheckout = "checkout"
	stepBuild    = "build"
	stepPull     = "secrets pull"
	stepRestart  = "restart"
	stepLink     = "link"
)

// autoUpdate is serve's self-update loop. Each tick asks which build an update
// would install; when that is not the build this process runs, it runs the
// install sequence `cairnos update` runs and then drains this process, leaving
// the restart onto the new binary to the supervisor.
type autoUpdate struct {
	app    *App
	drain  *api.Drain
	logger *slog.Logger
	opts   *updateOptions
	steps  UpdateSteps

	// attempted is the wanted revision the last install was for, also kept on
	// disk. Until that revision runs, a tick that wants it again skips the
	// install: a build that broke once breaks the same way, and each attempt
	// costs a compiler's worth of memory on a host that has run out before.
	attempted string

	// lookupErr is the last lookup failure logged, so one that repeats every
	// tick warns once.
	lookupErr string

	// handedOff is set once this loop has asked for the drain. The drain then
	// cancels serve's context as soon as nothing is in flight, and a step that
	// fails after it must still be logged rather than read as a shutdown.
	handedOff bool
}

// updateTarget is the build one tick would install and where it comes from: a
// published release, or the upstream commit of the checkout the running binary
// was built from. upstream is nil for a checkout that tracks nothing, which
// builds its own HEAD.
type updateTarget struct {
	release  *releaseBuild
	upstream *checkoutTarget
	dir      string
	wanted   string
}

// origin names where the target comes from, for the line that starts an update.
func (t *updateTarget) origin() string {
	if t.release != nil {
		return "release " + t.release.tag
	}

	return "checkout " + t.dir
}

// updateStepError tags a failure with the stage it came from. Error is the
// wrapped text alone, so the message reads as the step's own did.
type updateStepError struct {
	err  error
	step string
}

func (e *updateStepError) Error() string {
	return e.err.Error()
}

func (e *updateStepError) Unwrap() error {
	return e.err
}

// withStep tags err with the stage it came from, keeping a tag a step further
// in already gave it.
func withStep(step string, err error) error {
	if err == nil {
		return nil
	}

	if _, tagged := errors.AsType[*updateStepError](err); tagged {
		return err
	}

	return &updateStepError{step: step, err: err}
}

// stepOf reads back the stage withStep tagged, empty for an error it never saw.
func stepOf(err error) string {
	if stepErr, tagged := errors.AsType[*updateStepError](err); tagged {
		return stepErr.step
	}

	return ""
}

// named wraps each step so its failure carries the step's name, which is how
// the loop's error line names the stage without parsing the message.
func (s UpdateSteps) named() UpdateSteps {
	return UpdateSteps{
		Build: func(ctx context.Context, dir string) error {
			return withStep(stepBuild, s.Build(ctx, dir))
		},
		Install: func(target, staged string) error {
			return withStep(stepInstall, s.Install(target, staged))
		},
		Pull: func(ctx context.Context, binary, home string) (bool, error) {
			moved, err := s.Pull(ctx, binary, home)

			return moved, withStep(stepPull, err)
		},
		Restart: func(ctx context.Context, rebuilt, secretsMoved bool, wanted string) error {
			return withStep(stepRestart, s.Restart(ctx, rebuilt, secretsMoved, wanted))
		},
		Link: func(binary string) error {
			return withStep(stepLink, s.Link(binary))
		},
	}
}

// startAutoUpdate runs the [update] loop on its own goroutine when auto is on
// and a supervisor will start serve again once the loop ends it, closing the
// returned channel when the loop exits. Otherwise it starts nothing and returns
// nil, with one warning when auto was asked for.
func (a *App) startAutoUpdate(ctx context.Context, update *config.Update, home string, drain *api.Drain, logger *slog.Logger) <-chan struct{} {
	if !update.Auto {
		return nil
	}

	if reason := a.autoUpdateUnsupervised(ctx); reason != "" {
		logger.Warn("auto-update not started", "reason", reason)

		return nil
	}

	executable, err := executablePath()
	if err != nil {
		logger.Warn("auto-update not started", "error", err)

		return nil
	}

	opts := updateOptions{
		lookupLogger: slog.New(slog.DiscardHandler),
		home:         home,
		executable:   executable,
		running:      ownRevision(),
		fetch:        true,
		link:         true,
		release:      true,
	}

	loop := a.newAutoUpdate(&opts, drain, a.defaultSteps(&opts), logger)
	done := make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(update.Interval)
		defer ticker.Stop()

		loop.run(ctx, ticker.C)
	}()

	a.printOutf("auto-update checking every %s\n", update.Interval)

	return done
}

// autoUpdateUnsupervised answers through the App's hook when a suite set one,
// and asks systemd otherwise.
func (a *App) autoUpdateUnsupervised(ctx context.Context) string {
	if a.unsupervised != nil {
		return a.unsupervised(ctx)
	}

	return systemdUnsupervised(ctx)
}

// systemdUnsupervised names why nothing would start serve again after the loop
// ends it, empty when something would. Only a systemd unit qualifies: its
// Restart=always restarts a clean exit, where launchd's KeepAlive restarts a
// crash only. The main-process check keeps a serve run by hand beside an
// installed unit from updating the host and then staying down.
func systemdUnsupervised(ctx context.Context) string {
	host, err := service.DetectHost()
	if err != nil {
		return "could not read the host facts: " + err.Error()
	}

	for _, install := range service.Discover(host, unitFileExists) {
		if install.Systemd() && unitMainPID(ctx, install) == os.Getpid() {
			return ""
		}
	}

	return "serve is not the main process of an installed systemd service, so nothing would start it again after an update"
}

// newAutoUpdate wires the loop over one resolved invocation. Restart is always
// the loop's own, because draining this process is how the new build takes
// over; the other steps are the ones `cairnos update` runs.
func (a *App) newAutoUpdate(opts *updateOptions, drain *api.Drain, steps UpdateSteps, logger *slog.Logger) *autoUpdate {
	loop := &autoUpdate{app: a, drain: drain, logger: logger, opts: opts}

	steps.Restart = loop.restart
	loop.steps = steps.named()

	attempted, err := readAttempt(loop.attemptPath())
	if err != nil {
		logger.Warn("auto-update: could not read the last attempt, so its revision may be tried again", "error", err)
	}

	loop.attempted = attempted

	return loop
}

// run ticks until the loop finishes or serve shuts down. The first check waits
// a full interval, so a service that keeps crashing does not rebuild on every
// start.
func (u *autoUpdate) run(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}

		if u.tick(ctx) {
			return
		}
	}
}

// tick runs one check and reports whether the loop is finished, which it is
// once this process is draining for the new build or serve is shutting down.
// Ticks run one at a time on the loop's goroutine, so two updates never
// overlap inside one serve.
func (u *autoUpdate) tick(ctx context.Context) bool {
	if u.drain.Draining() {
		return true
	}

	lookupCtx, cancel := context.WithTimeout(ctx, autoUpdateTimeout)
	defer cancel()

	target, err := u.target(lookupCtx)
	if err != nil {
		return u.lookupFailed(ctx, err)
	}

	u.lookupErr = ""

	if !u.due(&target) {
		return false
	}

	unlock, err := lockUpdate(u.opts.home)
	if err != nil {
		u.lockFailed(err)

		return false
	}

	defer unlock()

	return u.update(ctx, &target)
}

// update runs the install one tick decided on, holding the update lock, and
// reports whether the loop is finished. The attempt is on disk before any of it
// starts, and a checkout the loop will not build is refused before the line
// that announces an update.
func (u *autoUpdate) update(ctx context.Context, target *updateTarget) bool {
	installCtx, cancel := context.WithTimeout(ctx, autoUpdateTimeout)
	defer cancel()

	u.recordAttempt(target.wanted)

	if err := target.ready(installCtx); err != nil {
		return u.installFailed(ctx, target, withStep(stepCheckout, err))
	}

	u.logger.Info("auto-update: updating",
		"from", cmp.Or(u.opts.running, unstampedRevision), "to", target.wanted, "via", target.origin())

	if err := u.install(installCtx, target); err != nil {
		return u.installFailed(ctx, target, err)
	}

	u.forgetAttempt()

	return true
}

// lockFailed logs a lock the loop could not take. Another update holding it is
// ordinary, and the next tick asks again; anything else is worth a warning.
func (u *autoUpdate) lockFailed(err error) {
	if errors.Is(err, errUpdateBusy) {
		u.logger.Debug("auto-update: another update is running, so this tick waits for the next", "error", err)

		return
	}

	u.logger.Warn("auto-update: could not take the update lock", "error", err)
}

// lookupFailed reports a lookup that named no wanted revision, and whether the
// loop is finished. A failure that repeats every tick warns once rather than
// every interval, and a shutdown cutting the lookup short is no failure at all.
func (u *autoUpdate) lookupFailed(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}

	if err.Error() == u.lookupErr {
		u.logger.Debug("auto-update: still cannot tell which build is wanted", "error", err)

		return false
	}

	u.logger.Warn("auto-update: could not tell which build is wanted", "error", err)
	u.lookupErr = err.Error()

	return false
}

// installFailed logs a failed install and reports whether the loop is finished.
// A shutdown cutting the install short is not the revision's fault, so the
// attempt is forgotten rather than logged. Once this loop has handed off, a
// canceled context is its own drain at work, and the failure is still logged.
func (u *autoUpdate) installFailed(ctx context.Context, target *updateTarget, err error) bool {
	if !u.handedOff && ctx.Err() != nil {
		u.forgetAttempt()

		return true
	}

	u.logger.Error("auto-update: a step failed", "step", stepOf(err), "wanted", target.wanted, "error", err)

	// Once the drain was asked for, the process ends on the new build anyway.
	return u.handedOff
}

// due reports whether target is worth an install: not the build already
// running, and not one an earlier attempt failed to bring up. The second guard
// keeps a broken build from being retried every tick; a new wanted revision
// clears it.
func (u *autoUpdate) due(target *updateTarget) bool {
	switch target.wanted {
	case u.opts.running:
		u.logger.Debug("auto-update: running the wanted build", "revision", target.wanted)

		return false
	case u.attempted:
		u.logger.Debug("auto-update: skipping a revision an earlier attempt did not bring up",
			"revision", target.wanted)

		return false
	default:
		return true
	}
}

// attemptPath is where the last attempted revision is kept.
func (u *autoUpdate) attemptPath() string {
	return filepath.Join(u.opts.home, autoUpdateAttemptName)
}

// recordAttempt notes the revision an install is starting for, on disk before
// the install begins, and success clears it again. A serve killed mid-build,
// which is how running the host out of memory can end, then skips that
// revision once systemd restarts it instead of building it again an interval
// later.
func (u *autoUpdate) recordAttempt(wanted string) {
	u.attempted = wanted

	if err := os.WriteFile(u.attemptPath(), []byte(wanted+"\n"), attemptFilePerm); err != nil {
		u.logger.Warn("auto-update: could not record the attempt, so a restart would try it again",
			"path", u.attemptPath(), "error", err)
	}
}

// forgetAttempt clears an attempt that succeeded, or one a shutdown cut short,
// which says nothing about whether the revision installs.
func (u *autoUpdate) forgetAttempt() {
	u.attempted = ""

	if err := os.Remove(u.attemptPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		u.logger.Warn("auto-update: could not clear the interrupted attempt", "path", u.attemptPath(), "error", err)
	}
}

// readAttempt loads the revision the last attempt was for, empty when no
// attempt was ever recorded.
func readAttempt(path string) (string, error) {
	content, err := os.ReadFile(path) // #nosec G304 -- a fixed name under the CairnOS home
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	return strings.TrimSpace(string(content)), nil
}

// target asks which build an update would install, deciding the way `cairnos
// update` does: the checkout's upstream when the running binary sits in one,
// and the latest release for this platform otherwise.
func (u *autoUpdate) target(ctx context.Context) (updateTarget, error) {
	build, found, err := u.app.findRelease(ctx, u.opts)
	if err != nil {
		return updateTarget{}, err
	}

	if found {
		return updateTarget{release: &build, wanted: build.revision}, nil
	}

	return upstreamTarget(ctx, u.opts)
}

// upstreamTarget names the commit the checkout the running binary was built
// from would build: its upstream's, after a fetch that moves only
// remote-tracking refs. A checkout tracking nothing wants its own HEAD, as it
// does for `cairnos update`, and has nothing to fetch.
func upstreamTarget(ctx context.Context, opts *updateOptions) (updateTarget, error) {
	dir, err := discoverSource(opts.source, opts.executable)
	if err != nil {
		return updateTarget{}, err
	}

	target := updateTarget{dir: dir}
	ref := headRef

	if upstream, found := resolveTarget(ctx, dir); found {
		if fetchErr := fetchTarget(ctx, dir, &upstream); fetchErr != nil {
			return updateTarget{}, fetchErr
		}

		target.upstream = &upstream
		ref = upstream.ref
	}

	target.wanted, err = commitOf(ctx, dir, ref)
	if err != nil {
		return updateTarget{}, err
	}

	return target, nil
}

// ready refuses a target the loop cannot turn into the wanted build. A release
// is always ready, and so is a checkout with an upstream, which the install
// fast-forwards or resets onto it; one tracking nothing has to be clean.
func (t *updateTarget) ready(ctx context.Context) error {
	if t.release != nil || t.upstream != nil {
		return nil
	}

	return requireClean(ctx, t.dir)
}

// install runs the sequence `cairnos update` runs for the target, with this
// loop's restart in place of the command's. The steps tag their own failures,
// so what arrives untagged is the release download and its checks, or the
// checkout reads around the build.
func (u *autoUpdate) install(ctx context.Context, target *updateTarget) error {
	if target.release != nil {
		return withStep(stepDownload, u.app.applyRelease(ctx, target.release, u.opts, u.steps))
	}

	if target.upstream != nil {
		if err := u.app.alignCheckout(ctx, target.dir, target.upstream); err != nil {
			return withStep(stepCheckout, err)
		}
	}

	return withStep(stepCheckout, applyUpdate(ctx, target.dir, u.opts, u.steps))
}

// requireClean refuses a dirty checkout that tracks no upstream. With nothing
// to reset onto, its build would be stamped "+dirty", read as stale after the
// restart, and be rebuilt on every start.
func requireClean(ctx context.Context, dir string) error {
	dirty, err := treeDirty(ctx, dir)
	if err != nil {
		return err
	}

	if dirty {
		return fmt.Errorf("%w: %s", errDirtyCheckout, dir)
	}

	return nil
}

// restart hands this process to the supervisor. The stamp check comes first,
// because a binary that is not the wanted revision would read as stale after
// the restart and be rebuilt on every start. The drain is the one
// RequestRestart throws, with the same ceiling, so in-flight runs finish first.
// A tick already cut short asks for no drain.
func (u *autoUpdate) restart(ctx context.Context, _, _ bool, wanted string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("drain for %s: %w", wanted, err)
	}

	stamp, err := stampedRevision(u.opts.executable)
	if err != nil {
		return err
	}

	if stamp != wanted {
		return fmt.Errorf("%w: %s is stamped %q, the update wanted %s",
			errBuildStamp, u.opts.executable, stamp, wanted)
	}

	deadline := u.drain.Request(api.DefaultDrainTimeout, time.Now())
	u.handedOff = true

	u.logger.Info("auto-update: draining; the supervisor starts the new build once in-flight runs finish",
		"revision", wanted, "deadline", deadline)

	return nil
}
