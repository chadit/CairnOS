package cli

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/execretry"
	"github.com/chadit/CairnOS/internal/service"
	"github.com/chadit/CairnOS/internal/source"
)

const updateUsage = `usage: cairnos update [flags]

Update this host in place. The latest GitHub release for this platform is
preferred: its asset is downloaded, checked against the published sha256, and
written over the running binary. With no release to use, the checkout the
running binary was built from is fast-forwarded and rebuilt instead. Either way
the new build pulls secrets, the service restarts when the revision or the
secrets moved, and ~/.local/bin/cairnos ends up pointing at the result.

  --source <dir>  build from this checkout instead of the one around the
                  running binary, which lives at <checkout>/bin/cairnos; this
                  also skips the release lookup
  --dir <dir>     CairnOS home the secrets pull writes into
  --check         report the running and the wanted revision, exit 3 when they
                  differ, and fetch, build, pull and restart nothing
  --restart-now   restart the service immediately instead of letting it finish
                  its in-flight runs first
  --no-fetch      build what is already checked out, reaching no release
  --no-release    skip the release lookup and build the checkout
  --no-link       leave ~/.local/bin/cairnos alone
`

// updateBinaryRel is where the build lands inside the checkout. It matches the
// Makefile's BINARY, so `make build` and this command cannot disagree.
const updateBinaryRel = "bin/cairnos"

// The per-user bin directory on the login PATH and the entry this command
// keeps pointed at the build.
const (
	linkDirRel = ".local/bin"
	linkName   = "cairnos"
)

// linkDirPerm is the mode ~/.local/bin gets when this command creates it.
const linkDirPerm = 0o755

// How long the restart waits for a new main process, and how often it asks.
const (
	restartTimeout = 30 * time.Second
	restartPoll    = time.Second
)

// dirtySuffix is what cmd/go appends to the stamped revision when the tree it
// built from carried any change, untracked files included.
const dirtySuffix = "+dirty"

// goFallbackPath is where the official tarball installs the toolchain. A
// systemd user session often starts with a PATH that omits it.
const goFallbackPath = "/usr/local/go/bin/go"

// goosLinux is the only platform that exposes a running process image, which
// is where the revision comparison reads the running build from.
const goosLinux = "linux"

// procExeFormat spells the running process image's path. It is a format rather
// than a filepath.Join because those components are separators the host fixes,
// not a path this command assembles.
const procExeFormat = "/proc/%d/exe"

// updateOptions carries one invocation's resolved inputs, so each step takes a
// single value rather than the same seven parameters. executable and apiBase
// are resolved rather than parsed: a test names the binary to replace and the
// server to ask, where the command names its own image and api.github.com.
type updateOptions struct {
	// transport carries the release lookup and the asset download. Nil is the
	// default one, so a deploy opens real sockets; a test passes one that
	// answers in process, which keeps the suite off the host's network stack.
	transport http.RoundTripper

	source     string
	home       string
	executable string
	apiBase    string

	check   bool
	fetch   bool
	link    bool
	release bool

	// restartNow skips the drain and restarts the service the moment the new
	// binary is in place, which is what an operator wants when the running
	// build is the problem.
	restartNow bool
}

// UpdateSteps holds the side-effecting halves of the update. They are function
// values so a test can pin the order they run in without a toolchain, a vault,
// or a service unit, which is the guarantee FR11 rests on. Build belongs to the
// source path and Install to the release path; every run uses one of the two.
type UpdateSteps struct {
	Build   func(ctx context.Context, dir string) error
	Install func(target, staged string) error
	Pull    func(ctx context.Context, binary, home string) (bool, error)
	Restart func(ctx context.Context, rebuilt, secretsMoved bool, wanted string) error
	Link    func(binary string) error
}

// defaultSteps wires the real steps, which is everything the command does
// besides reading revisions. Restart closes over the invocation because the
// step signature carries the revision pair, not the flags that decide whether
// the service drains first.
func (a *App) defaultSteps(opts *updateOptions) UpdateSteps {
	return UpdateSteps{
		Build:   a.buildBinary,
		Install: installBinary,
		Pull:    a.pullSecrets,
		Restart: func(ctx context.Context, rebuilt, secretsMoved bool, wanted string) error {
			return a.restartWhenStale(ctx, opts, rebuilt, secretsMoved, wanted)
		},
		Link: a.linkBinary,
	}
}

// cmdUpdate updates this host in place. The step order is the point: the pull
// runs the binary that was just built, and the restart comes after it, so the
// service starts on both the new build and the new values.
func (a *App) cmdUpdate(ctx context.Context, args []string) int {
	const label = "update"

	flags := flag.NewFlagSet(label, flag.ContinueOnError)
	flags.SetOutput(a.stderr)
	flags.Usage = func() { a.printErrf("%s", updateUsage) }

	sourceFlag := flags.String("source", "", "checkout to build from")
	dirFlag := dirFlagOn(flags)
	checkFlag := flags.Bool("check", false, "report the revisions and exit 3 when they differ")
	restartNowFlag := flags.Bool("restart-now", false, "restart at once instead of draining first")
	noFetchFlag := flags.Bool("no-fetch", false, "build what is already checked out")
	noReleaseFlag := flags.Bool("no-release", false, "skip the release lookup")
	noLinkFlag := flags.Bool("no-link", false, "leave ~/.local/bin/cairnos alone")

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	home, err := resolveDir(*dirFlag, a.envLookup())
	if err != nil {
		a.printCmdErr(label, err)

		return ExitFailure
	}

	executable, err := executablePath()
	if err != nil {
		a.printCmdErr(label, err)

		return ExitFailure
	}

	opts := updateOptions{
		source:     *sourceFlag,
		home:       home,
		executable: executable,
		check:      *checkFlag,
		fetch:      !*noFetchFlag,
		link:       !*noLinkFlag,
		release:    !*noReleaseFlag,
		restartNow: *restartNowFlag,
	}

	code, err := a.runUpdate(ctx, &opts, a.defaultSteps(&opts))
	if err != nil {
		a.printCmdErr(label, err)

		return ExitFailure
	}

	return code
}

// runUpdate picks between the release and the source path and then either
// reports or applies. A release wins when one exists for this platform, since
// installing a published build beats rebuilding the same commit on the host.
func (a *App) runUpdate(ctx context.Context, opts *updateOptions, steps UpdateSteps) (int, error) {
	build, found, err := a.findRelease(ctx, opts)
	if err != nil {
		return ExitFailure, err
	}

	if found {
		return a.runRelease(ctx, &build, opts, steps)
	}

	return a.runSource(ctx, opts, steps)
}

// runRelease reports or installs the release. The wanted revision is the commit
// its tag points at, which is what the stamp in the running process is compared
// against.
func (a *App) runRelease(ctx context.Context, build *releaseBuild, opts *updateOptions, steps UpdateSteps) (int, error) {
	a.logger.Info("update: latest release", "tag", build.tag, "asset", build.assetName)

	if opts.check {
		return a.reportRevisionPair(a.runningRevision(ctx), build.revision), nil
	}

	return ExitOK, a.applyRelease(ctx, build, opts, steps)
}

// runSource is the fallback and the developer path: fast-forward the checkout
// the running binary was built from and rebuild it. --check stops at the
// report, which is what makes it safe on a host somebody only wants to look at.
func (a *App) runSource(ctx context.Context, opts *updateOptions, steps UpdateSteps) (int, error) {
	dir, err := discoverSource(opts.source, opts.executable)
	if err != nil {
		return ExitFailure, err
	}

	a.logger.Info("update: source checkout", "dir", dir)

	if opts.check {
		return a.reportSource(ctx, dir)
	}

	if opts.fetch {
		if syncErr := a.syncCheckout(ctx, dir); syncErr != nil {
			return ExitFailure, syncErr
		}
	}

	return ExitOK, a.applyUpdate(ctx, dir, opts, steps)
}

// applyRelease installs the published binary and then runs the same pull,
// restart and link the source path does. A release whose commit the service
// already runs installs nothing and still pulls, because a vault value can move
// without the code moving.
func (a *App) applyRelease(ctx context.Context, build *releaseBuild, opts *updateOptions, steps UpdateSteps) error {
	running := a.runningRevision(ctx)

	a.logger.Info("update: revisions", "running", displayRevision(running), "wanted", build.revision)

	replaced := running != build.revision

	if replaced {
		if err := a.installRelease(ctx, build, opts, steps); err != nil {
			return err
		}
	}

	secretsMoved, err := steps.Pull(ctx, opts.executable, opts.home)
	if err != nil {
		return err
	}

	if restartErr := steps.Restart(ctx, replaced, secretsMoved, build.revision); restartErr != nil {
		return restartErr
	}

	if !opts.link {
		return nil
	}

	return steps.Link(opts.executable)
}

// installRelease downloads the asset into a staging directory, checks it
// against the published sha256 and its own build stamp, and only then replaces
// the running binary.
func (a *App) installRelease(ctx context.Context, build *releaseBuild, opts *updateOptions, steps UpdateSteps) error {
	dir, err := os.MkdirTemp("", "cairnos-update-")
	if err != nil {
		return fmt.Errorf("create a staging directory: %w", err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	staged, err := a.releaseClient(opts.apiBase, opts.transport).stageAsset(ctx, build, dir)
	if err != nil {
		return err
	}

	if stampErr := verifyStamp(staged, build.revision); stampErr != nil {
		return stampErr
	}

	a.logger.Info("update: installing the release", "tag", build.tag, "binary", opts.executable)

	return steps.Install(opts.executable, staged)
}

// applyUpdate builds, pulls, restarts, and relinks, keeping runUpdate to the
// two steps --check shares with it. The pull sits before the restart because
// the service reads both environment files once, at startup: pulling after
// would leave the new process running on the previous values (FR11).
func (a *App) applyUpdate(ctx context.Context, dir string, opts *updateOptions, steps UpdateSteps) error {
	running := a.runningRevision(ctx)

	wanted, err := wantedRevision(ctx, dir)
	if err != nil {
		return err
	}

	a.logger.Info("update: revisions", "running", displayRevision(running), "wanted", wanted)

	if buildErr := steps.Build(ctx, dir); buildErr != nil {
		return buildErr
	}

	binary := filepath.Join(dir, updateBinaryRel)

	secretsMoved, err := steps.Pull(ctx, binary, opts.home)
	if err != nil {
		return err
	}

	if restartErr := steps.Restart(ctx, running != wanted, secretsMoved, wanted); restartErr != nil {
		return restartErr
	}

	if !opts.link {
		return nil
	}

	return steps.Link(binary)
}

// reportSource prints what the service runs beside what the checkout carries.
func (a *App) reportSource(ctx context.Context, dir string) (int, error) {
	wanted, err := wantedRevision(ctx, dir)
	if err != nil {
		return ExitFailure, err
	}

	return a.reportRevisionPair(a.runningRevision(ctx), wanted), nil
}

// reportRevisionPair prints the running build beside the one the update would
// put there. A dirty wanted revision always counts as different: two builds
// both stamped "+dirty" say nothing about whether the same uncommitted change
// is in the running one.
func (a *App) reportRevisionPair(running, wanted string) int {
	a.printOutf("running: %s\nwanted:  %s\n", displayRevision(running), wanted)

	if running == wanted && !strings.HasSuffix(wanted, dirtySuffix) {
		return ExitOK
	}

	return ExitBehind
}

// displayRevision names an absent running build, since a blank field reads as
// a broken check rather than a stopped service.
func displayRevision(revision string) string {
	if revision == "" {
		return "not running"
	}

	return revision
}

// discoverSource resolves the checkout to update. The default is the parent of
// the running binary's directory, because the build lands at
// <checkout>/bin/cairnos; --source names a checkout the binary never came from.
func discoverSource(override, executable string) (string, error) {
	if override != "" {
		return checkoutDir(override)
	}

	return checkoutDir(filepath.Dir(filepath.Dir(executable)))
}

// checkoutDir accepts a directory only when it carries both go.mod and .git,
// the two marks of a clone this command can fetch and build.
func checkoutDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", dir, err)
	}

	for _, mark := range []string{"go.mod", ".git"} {
		if _, statErr := os.Stat(filepath.Join(abs, mark)); statErr != nil {
			return "", fmt.Errorf("%w: %s carries no %s", errNoSourceCheckout, abs, mark)
		}
	}

	return abs, nil
}

// upstreamGap is how far a checkout sits from the branch it tracks.
type upstreamGap struct {
	ref    string
	behind int
	ahead  int
}

// syncCheckout fetches and fast-forwards only a checkout that is strictly
// behind. One that has diverged or carries local commits is left alone, since
// this command updates a host rather than resolving anyone's history. A dirty
// tree is fine: the build stamps "+dirty" and the revision line shows it.
func (a *App) syncCheckout(ctx context.Context, dir string) error {
	if _, err := source.GitCommand(ctx, dir, "fetch", "--quiet", "origin"); err != nil {
		return fmt.Errorf("fetch origin in %s: %w", dir, err)
	}

	gap, err := upstreamGapOf(ctx, dir)
	if err != nil {
		return err
	}

	if gap.ref == "" {
		a.logger.Info("update: the checkout tracks no upstream, leaving it as is", "dir", dir)

		return nil
	}

	if gap.behind == 0 || gap.ahead > 0 {
		a.logger.Info("update: checkout left as is",
			"upstream", gap.ref, "behind", gap.behind, "ahead", gap.ahead)

		return nil
	}

	if _, err := source.GitCommand(ctx, dir, "merge", "--ff-only", gap.ref); err != nil {
		return fmt.Errorf("fast-forward %s onto %s: %w", dir, gap.ref, err)
	}

	a.logger.Info("update: fast-forwarded", "upstream", gap.ref, "commits", gap.behind)

	return nil
}

// upstreamGapOf counts the commits each side holds that the other does not. An
// empty ref means the branch tracks nothing, which is not a failure: a host can
// sit on a detached HEAD and still deserve a build.
func upstreamGapOf(ctx context.Context, dir string) (upstreamGap, error) {
	ref := upstreamRef(ctx, dir)
	if ref == "" {
		return upstreamGap{}, nil
	}

	counts, err := source.GitCommand(ctx, dir, "rev-list", "--left-right", "--count", "HEAD..."+ref)
	if err != nil {
		return upstreamGap{}, fmt.Errorf("count commits against %s in %s: %w", ref, dir, err)
	}

	ahead, behind, err := parseGapCounts(counts)
	if err != nil {
		return upstreamGap{}, err
	}

	return upstreamGap{ref: ref, behind: behind, ahead: ahead}, nil
}

// upstreamRef names the branch the checkout tracks, empty when it tracks none.
// A non-zero exit from git is the answer rather than a problem: that is how it
// reports a branch with no upstream and a detached HEAD alike.
func upstreamRef(ctx context.Context, dir string) string {
	ref, err := source.GitCommand(ctx, dir,
		"rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	if err != nil {
		return ""
	}

	return ref
}

// parseGapCounts reads `git rev-list --left-right --count`, whose two fields
// are the commits only on the left of the range and only on the right.
func parseGapCounts(line string) (int, int, error) {
	fields := strings.Fields(line)
	if len(fields) != 2 { //nolint:mnd // the command prints exactly two counts
		return 0, 0, fmt.Errorf("%w: %q", errGitCountShape, line)
	}

	left, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %q", errGitCountShape, line)
	}

	right, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %q", errGitCountShape, line)
	}

	return left, right, nil
}

// wantedRevision spells the checkout's HEAD the way cmd/go stamps it, with
// "+dirty" on any change at all, untracked files included, because that is what
// the plain `git status --porcelain` cmd/go runs reports.
func wantedRevision(ctx context.Context, dir string) (string, error) {
	head, err := source.GitCommand(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read HEAD in %s: %w", dir, err)
	}

	status, err := source.GitCommand(ctx, dir, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("read the working tree state in %s: %w", dir, err)
	}

	if status != "" {
		return head + dirtySuffix, nil
	}

	return head, nil
}

// runningRevision reads the stamp out of the service's own process image, so a
// binary already replaced on disk is never mistaken for the one in memory. An
// empty result means there is nothing running to compare against.
func (a *App) runningRevision(ctx context.Context) string {
	install, found := a.serveInstall()
	if !found {
		return ""
	}

	pid := unitMainPID(ctx, install)
	if pid == 0 {
		return ""
	}

	return a.pidRevision(pid)
}

// pidRevision reads the stamp out of a running process image. Linux is the only
// host with /proc, so elsewhere there is nothing to read and the caller treats
// the service as unverifiable rather than stale.
func (a *App) pidRevision(pid int) string {
	if runtime.GOOS != goosLinux {
		return ""
	}

	revision, err := stampedRevision(fmt.Sprintf(procExeFormat, pid))
	if err != nil {
		a.logger.Warn("update: could not read the running build", "pid", pid, "error", err)

		return ""
	}

	return revision
}

// stampedRevision reads the vcs stamp Go embeds in a binary: "<sha>" or
// "<sha>+dirty", and an empty string for a binary built outside a repository.
func stampedRevision(path string) (string, error) {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the build stamp of %s: %w", path, err)
	}

	var (
		revision string
		dirty    bool
	)

	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}

	if revision != "" && dirty {
		return revision + dirtySuffix, nil
	}

	return revision, nil
}

// serveInstall picks the systemd install this host runs serve under. launchd
// reports no main process id the way systemd does, so a macOS host takes the
// no-install path and keeps its restart by hand.
func (a *App) serveInstall() (service.Install, bool) {
	host, err := service.DetectHost()
	if err != nil {
		a.logger.Warn("update: could not read the host facts", "error", err)

		return service.Install{}, false
	}

	for _, install := range service.Discover(host, unitFileExists) {
		if install.Systemd() {
			return install, true
		}
	}

	return service.Install{}, false
}

// unitFileExists reports whether a unit file is present, which is all
// service.Discover asks about one.
func unitFileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

// unitMainPID asks the supervisor for the unit's main process id, and answers 0
// for a unit that is unknown, stopped, or mid-restart.
func unitMainPID(ctx context.Context, install service.Install) int {
	output, err := service.ExecCommand(ctx, install.MainPIDArgv())
	if err != nil {
		return 0
	}

	pid, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil {
		return 0
	}

	return pid
}

// buildBinary rebuilds the checkout. make is preferred because the Makefile's
// build target is the one a developer runs, so the two cannot drift; the go
// commands below mirror that target for a host with no make.
func (a *App) buildBinary(ctx context.Context, dir string) error {
	a.logger.Info("update: building", "dir", dir, "binary", updateBinaryRel)

	if makeBin, err := environ.LookPath(a.envLookup(), "make"); err == nil {
		return runInDir(ctx, dir, makeBin, "--no-print-directory", "build")
	}

	goBin, err := lookGoToolchain(a.envLookup())
	if err != nil {
		return err
	}

	if err := runInDir(ctx, dir, goBin, "tool", "buf", "generate"); err != nil {
		return err
	}

	return runInDir(ctx, dir, goBin, "build", "-o", updateBinaryRel, "./cmd/cairnos")
}

// lookGoToolchain finds go on PATH and falls back to the tarball's install
// prefix, which a systemd user session's PATH often omits.
func lookGoToolchain(lookup environ.Lookup) (string, error) {
	if path, err := environ.LookPath(lookup, "go"); err == nil {
		return path, nil
	}

	if _, err := os.Stat(goFallbackPath); err == nil {
		return goFallbackPath, nil
	}

	return "", errNoGoToolchain
}

// runInDir runs one build or pull command inside dir and folds the command's
// own output into the failure, so a broken build says what broke.
func runInDir(ctx context.Context, dir, name string, args ...string) error {
	output, err := execretry.CombinedOutput(ctx, func() (*exec.Cmd, error) {
		// #nosec G204 -- name is make, the go toolchain, or the binary this command
		// just built, and every argument is compiled in. No workflow, config, or
		// network input reaches here.
		command := exec.CommandContext(ctx, name, args...)
		command.Dir = dir

		return command, nil
	})
	if err != nil {
		return fmt.Errorf("run %s: %w: %s", name, err, strings.TrimSpace(string(output)))
	}

	return nil
}

// pullSecrets runs the new build's own pull and reports whether the generated
// file moved. Running the build rather than this process is what keeps the pull
// honest: a field the new code reads differently is rendered by the code that
// will read it.
func (a *App) pullSecrets(ctx context.Context, binary, home string) (bool, error) {
	path := service.SecretsEnvFile(home, false)

	before, err := fileDigest(path)
	if err != nil {
		return false, err
	}

	a.logger.Info("update: pulling secrets", "home", home)

	if pullErr := runInDir(ctx, filepath.Dir(binary), binary,
		"secrets", "pull", "--"+flagDir, home); pullErr != nil {
		return false, fmt.Errorf("secrets pull: %w", pullErr)
	}

	after, err := fileDigest(path)
	if err != nil {
		return false, err
	}

	return before != after, nil
}

// fileDigest is the sha256 of a file, and an empty string when it is absent. A
// host with no vault block never grows the file, which is ordinary rather than
// a failure.
func fileDigest(path string) (string, error) {
	content, err := os.ReadFile(path) // #nosec G304 -- the path is built from the resolved CairnOS home
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	sum := sha256.Sum256(content)

	return hex.EncodeToString(sum[:]), nil
}

// restartWhenStale turns the service over only when the build or the secrets
// moved, so an already current host keeps its in-flight runs instead of losing
// them to a restart that would change nothing.
func (a *App) restartWhenStale(ctx context.Context, opts *updateOptions, rebuilt, secretsMoved bool, wanted string) error {
	install, found := a.serveInstall()
	if !found {
		a.logger.Info("update: no service is installed here, nothing to restart")

		return nil
	}

	if !rebuilt && !secretsMoved {
		a.logger.Info("update: nothing moved, leaving the service running", "revision", wanted)

		return nil
	}

	previous := unitMainPID(ctx, install)

	drained, err := a.driveRestart(ctx, a.restartHooksFor(install, opts.home), opts.restartNow)
	if err != nil {
		return err
	}

	pid, err := waitForRestart(ctx, install, previous, restartWindow(drained))
	if err != nil {
		return err
	}

	return a.verifyRunning(pid, wanted)
}

// waitForRestart returns the unit's new main process id once it is active on a
// pid that is not the one it had before. window is how long that may take,
// which a drain stretches by the ceiling the service is finishing its runs in.
func waitForRestart(ctx context.Context, install service.Install, previous int, window time.Duration) (int, error) {
	deadline := time.Now().Add(window)

	for time.Now().Before(deadline) {
		if pid := activePID(ctx, install, previous); pid != 0 {
			return pid, nil
		}

		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("wait for %s: %w", install.Label, ctx.Err())
		case <-time.After(restartPoll):
		}
	}

	return 0, fmt.Errorf("%w: %s did not return within %s", errServiceSlowStart, install.Label, window)
}

// activePID answers with the unit's new main process id, and 0 while it is
// still the previous process or still starting.
func activePID(ctx context.Context, install service.Install, previous int) int {
	pid := unitMainPID(ctx, install)
	if pid == 0 || pid == previous {
		return 0
	}

	if _, err := service.ExecCommand(ctx, install.StatusArgv); err != nil {
		return 0
	}

	return pid
}

// verifyRunning confirms the service came back on the build this run produced,
// so a restart that quietly reused an old image fails rather than exits clean.
// A host with no readable process image is taken on trust.
func (a *App) verifyRunning(pid int, wanted string) error {
	running := a.pidRevision(pid)
	if running == "" {
		a.logger.Info("update: service restarted", "pid", pid)

		return nil
	}

	if running != wanted {
		return fmt.Errorf("%w: running %s, wanted %s", errServiceStale, running, wanted)
	}

	a.logger.Info("update: the service is running the new build", "pid", pid, "revision", running)

	return nil
}

// linkBinary keeps ~/.local/bin/cairnos pointed at the build, so a login shell
// runs what this command just made. Windows has no such entry on PATH, so the
// step is skipped there.
func (a *App) linkBinary(binary string) error {
	if runtime.GOOS == "windows" {
		return nil
	}

	home, err := homeDir(a.envLookup())
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}

	dir := filepath.Join(home, linkDirRel)

	if err := ensureLink(binary, dir); err != nil {
		return err
	}

	a.logger.Info("update: linked", "link", filepath.Join(dir, linkName), "target", binary)

	if !onPath(dir, a.envLookup().Get("PATH")) {
		a.logger.Warn("update: the link directory is not on PATH", "dir", dir)
	}

	return nil
}

// ensureLink points linkDir/cairnos at the binary, replacing a symlink aimed
// somewhere else. A regular file is refused: it may be an install this command
// never placed, and removing it would be somebody else's surprise.
func ensureLink(binary, linkDir string) error {
	// #nosec G301 -- ~/.local/bin sits on the login PATH and holds no secrets;
	// 0755 is the mode every other installer gives it.
	if err := os.MkdirAll(linkDir, linkDirPerm); err != nil {
		return fmt.Errorf("create %s: %w", linkDir, err)
	}

	link := filepath.Join(linkDir, linkName)

	info, err := os.Lstat(link)

	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("inspect %s: %w", link, err)
	case info.Mode()&fs.ModeSymlink == 0:
		return fmt.Errorf("%w: %s", errLinkNotASymlink, link)
	default:
		if removeErr := os.Remove(link); removeErr != nil {
			return fmt.Errorf("replace %s: %w", link, removeErr)
		}
	}

	if err := os.Symlink(binary, link); err != nil {
		return fmt.Errorf("link %s to %s: %w", link, binary, err)
	}

	return nil
}

// onPath reports whether dir is one of the PATH entries, so the warning fires
// only when the link would go unfound.
func onPath(dir, pathEnv string) bool {
	return slices.Contains(filepath.SplitList(pathEnv), dir)
}
