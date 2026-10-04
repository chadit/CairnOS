package cli_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/config"
)

// errBuildBroken stands in for a build that fails the same way every time it
// runs, which is the case the loop must not retry on every tick. errLinkBroken
// is a link that fails after the new build is already in place.
var (
	errBuildBroken = errors.New("build broke")
	errLinkBroken  = errors.New("link broke")
)

// staleRevision is a running build no fixture ever wants, so every case that
// uses it is behind.
const staleRevision = "0000000000000000000000000000000000000000"

// TestAutoUpdateLeavesACurrentBuildAlone covers the tick nearly every tick is:
// the upstream is the build already running, so nothing is fetched into the
// working tree, built, or drained.
func TestAutoUpdateLeavesACurrentBuildAlone(t *testing.T) {
	t.Parallel()
	requireGit(t)

	work, upstream := stageClonePair(t)
	current := commitAndPush(t, upstream, "upstream.txt")
	before := gitOut(t, work, "rev-parse", "HEAD")

	var order []string

	drain := api.NewDrain()
	steps := recordingSteps(t, &order, "")
	loop := cli.New(&bytes.Buffer{}, &bytes.Buffer{}).NewAutoUpdateLoop(sourceRun(t, work, current), drain, steps)

	if loop.Tick(t.Context()) {
		t.Fatal("the loop finished with nothing to install")
	}

	if len(order) != 0 {
		t.Errorf("steps ran for a current build: %v", order)
	}

	if drain.Draining() {
		t.Error("a current build was drained")
	}

	if head := gitOut(t, work, "rev-parse", "HEAD"); head != before {
		t.Errorf("HEAD moved to %s, want it left at %s", head, before)
	}
}

// TestAutoUpdateInstallsTheReleaseAndDrains covers a host behind the latest
// release: the verified asset lands over the running binary, the pull and link
// run, and the process drains itself for the supervisor to restart.
func TestAutoUpdateInstallsTheReleaseAndDrains(t *testing.T) {
	t.Parallel()
	requireGoBuild(t)

	asset, revision := buildStampedBinary(t)
	releases := startReleaseServer(t, defaultStagedRelease(asset, revision))

	target := filepath.Join(t.TempDir(), updateLinkName)
	writeFile(t, target, updateOldContent)

	var (
		order  []string
		stderr bytes.Buffer
	)

	drain := api.NewDrain()
	loop := cli.New(&bytes.Buffer{}, &stderr).NewAutoUpdateLoop(&cli.UpdateRun{
		APIBase:    releases.base,
		Transport:  releases.transport,
		Executable: target,
		Home:       t.TempDir(),
		Running:    staleRevision,
	}, drain, recordingSteps(t, &order, target))

	if !loop.Tick(t.Context()) {
		t.Fatal("the loop kept going after installing the new build")
	}

	if !drain.Draining() {
		t.Fatal("the new build was installed without draining the process")
	}

	want := []string{stepInstall, stepPull, stepLink}
	if !slices.Equal(order, want) {
		t.Errorf("step order = %v, want %v", order, want)
	}

	// One line names both revisions, and nothing else repeats the pair.
	for _, field := range []string{"auto-update: updating", "from=" + staleRevision, "to=" + revision} {
		if got := strings.Count(stderr.String(), field); got != 1 {
			t.Errorf("%q appears %d times, want once:\n%s", field, got, stderr.String())
		}
	}

	installed, err := os.ReadFile(target) // #nosec G304 -- a path this test built
	if err != nil {
		t.Fatalf("read back %s: %v", target, err)
	}

	if !bytes.Equal(installed, asset) {
		t.Errorf("the installed binary is %d bytes, the asset is %d", len(installed), len(asset))
	}
}

// TestAutoUpdateDoesNotRetryAFailedRevision pins the guard against a build that
// breaks every time: one attempt per wanted revision, no drain, and a new
// attempt only once the upstream moves.
func TestAutoUpdateDoesNotRetryAFailedRevision(t *testing.T) {
	t.Parallel()
	requireGit(t)

	work, upstream := stageClonePair(t)
	first := commitAndPush(t, upstream, "first.txt")

	var builds int

	steps := cli.UpdateSteps{
		Build: func(context.Context, string) error {
			builds++

			return errBuildBroken
		},
	}

	var stderr bytes.Buffer

	drain := api.NewDrain()
	loop := cli.New(&bytes.Buffer{}, &stderr).NewAutoUpdateLoop(sourceRun(t, work, staleRevision), drain, steps)

	if loop.Tick(t.Context()) {
		t.Fatal("the loop finished after a failed build")
	}

	if builds != 1 || drain.Draining() {
		t.Fatalf("after the failed build: builds = %d, draining = %t; want 1 build and no drain",
			builds, drain.Draining())
	}

	for _, want := range []string{"level=ERROR", "step=build", "wanted=" + first} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("the failure log is missing %q:\n%s", want, stderr.String())
		}
	}

	loop.Tick(t.Context())

	if builds != 1 {
		t.Fatalf("the failed revision was built again: builds = %d", builds)
	}

	second := commitAndPush(t, upstream, "second.txt")

	loop.Tick(t.Context())

	if builds != 2 {
		t.Fatalf("builds = %d after the upstream moved to %s, want a second attempt", builds, second)
	}

	if drain.Draining() {
		t.Error("a failed build drained the process")
	}
}

// TestAutoUpdateResetsACheckoutItCannotFastForward covers the two checkouts a
// fast-forward cannot carry. The loop resets each onto the upstream, warns with
// what it discarded, and builds a binary stamped with the upstream commit, so
// the drain goes ahead. An untracked file left behind would stamp that build
// "+dirty" and stop it at the restart.
func TestAutoUpdateResetsACheckoutItCannotFastForward(t *testing.T) {
	t.Parallel()
	requireGoBuild(t)

	cases := []struct {
		stage func(t *testing.T, work string)
		name  string
	}{
		{
			name: "a dirty working tree",
			stage: func(t *testing.T, work string) {
				t.Helper()
				writeFile(t, filepath.Join(work, "main.go"), stagedModuleMain+"// wip\n")
				writeFile(t, filepath.Join(work, "scratch.txt"), "wip\n")
			},
		},
		{
			name:  "local commits the upstream lacks",
			stage: func(t *testing.T, work string) { t.Helper(); commitLocal(t, work, "local.txt") },
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			work, upstream := stageClonePair(t)
			wanted := commitAndPush(t, upstream, "upstream.txt")
			testCase.stage(t, work)

			var (
				order  []string
				stderr bytes.Buffer
			)

			steps := recordingSteps(t, &order, "")
			steps.Build = func(ctx context.Context, dir string) error {
				order = append(order, stepBuild)

				return goBuildStamped(ctx, dir, filepath.Join(dir, "bin", updateLinkName))
			}

			drain := api.NewDrain()
			loop := cli.New(&bytes.Buffer{}, &stderr).NewAutoUpdateLoop(sourceRun(t, work, staleRevision), drain, steps)

			if !loop.Tick(t.Context()) || !drain.Draining() {
				t.Fatalf("the reset checkout was not built through to the drain:\n%s", stderr.String())
			}

			if head := gitOut(t, work, "rev-parse", "HEAD"); head != wanted {
				t.Errorf("HEAD = %s, want the upstream commit %s", head, wanted)
			}

			for _, want := range []string{"level=WARN", "checkout reset onto its upstream"} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("the reset log is missing %q:\n%s", want, stderr.String())
				}
			}
		})
	}
}

// TestAutoUpdateRefusesADirtyCheckoutWithNoUpstream covers the one checkout the
// loop still refuses. With no upstream to reset onto, its build would be stamped
// "+dirty" and read as stale after every restart, so nothing is built and the
// tree is left as it is.
func TestAutoUpdateRefusesADirtyCheckoutWithNoUpstream(t *testing.T) {
	t.Parallel()
	requireGit(t)

	work, _ := stageClonePair(t)
	gitOut(t, work, "remote", "remove", "origin")

	scratch := filepath.Join(work, "scratch.txt")
	writeFile(t, scratch, "wip\n")

	var (
		order  []string
		stderr bytes.Buffer
	)

	drain := api.NewDrain()
	steps := recordingSteps(t, &order, "")
	loop := cli.New(&bytes.Buffer{}, &stderr).NewAutoUpdateLoop(sourceRun(t, work, staleRevision), drain, steps)

	if loop.Tick(t.Context()) || drain.Draining() {
		t.Fatal("the loop drained after refusing the checkout")
	}

	if len(order) != 0 {
		t.Errorf("steps ran on a checkout the loop refused: %v", order)
	}

	if !strings.Contains(stderr.String(), "step=checkout") {
		t.Errorf("the failure log does not name the checkout step:\n%s", stderr.String())
	}

	if strings.Contains(stderr.String(), "auto-update: updating") {
		t.Errorf("an update was announced for a checkout the loop refused:\n%s", stderr.String())
	}

	if _, err := os.Stat(scratch); err != nil {
		t.Errorf("the refused checkout lost its untracked file: %v", err)
	}
}

// TestAutoUpdateWillNotRestartOntoTheWrongBuild covers a build that finished
// but carries another revision's stamp. Restarting onto it would read as stale
// on the next start and rebuild every time, so the drain is never asked for.
func TestAutoUpdateWillNotRestartOntoTheWrongBuild(t *testing.T) {
	t.Parallel()
	requireGoBuild(t)

	work, upstream := stageClonePair(t)
	commitAndPush(t, upstream, "upstream.txt")

	// A real binary, stamped with its own fixture repository's commit rather
	// than the upstream one the loop wants.
	stranger, _ := buildStampedBinary(t)
	executable := filepath.Join(work, "bin", updateLinkName)

	var (
		order  []string
		stderr bytes.Buffer
	)

	steps := recordingSteps(t, &order, "")
	steps.Build = func(context.Context, string) error {
		order = append(order, stepBuild)

		return os.WriteFile(executable, stranger, 0o700) // #nosec G306 -- an executable fixture
	}

	if err := os.MkdirAll(filepath.Dir(executable), 0o750); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(executable), err)
	}

	drain := api.NewDrain()
	loop := cli.New(&bytes.Buffer{}, &stderr).NewAutoUpdateLoop(sourceRun(t, work, staleRevision), drain, steps)

	if loop.Tick(t.Context()) || drain.Draining() {
		t.Fatal("the loop drained onto a build stamped with another revision")
	}

	want := []string{stepBuild, stepPull}
	if !slices.Equal(order, want) {
		t.Errorf("step order = %v, want %v with no link after the refused restart", order, want)
	}

	if !strings.Contains(stderr.String(), "step=restart") {
		t.Errorf("the failure log does not name the restart step:\n%s", stderr.String())
	}
}

// TestAutoUpdateStartsOnlyWhenAskedAndSupervised covers serve's decision to
// start the loop. Off is silent. On without a supervisor warns once and names
// why, since a drained serve with nothing to restart it would stay down. On
// under a supervisor starts the loop, which ends with serve.
func TestAutoUpdateStartsOnlyWhenAskedAndSupervised(t *testing.T) {
	t.Parallel()

	const noSupervisor = "no unit runs this process"

	cases := []struct {
		name         string
		unsupervised string
		update       config.Update
		wantStart    bool
		wantWarn     bool
	}{
		{name: "auto off", update: config.Update{Interval: 15 * time.Minute}},
		{
			name:         "auto on with no supervisor",
			update:       config.Update{Auto: true, Interval: 15 * time.Minute},
			unsupervised: noSupervisor,
			wantWarn:     true,
		},
		{
			name:      "auto on under a supervisor",
			update:    config.Update{Auto: true, Interval: 15 * time.Minute},
			wantStart: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx, stopServe := context.WithCancel(t.Context())
			defer stopServe()

			var stderr bytes.Buffer

			done := cli.New(&bytes.Buffer{}, &stderr).
				RunAutoUpdateStart(ctx, &testCase.update, t.TempDir(), testCase.unsupervised)

			if started := done != nil; started != testCase.wantStart {
				t.Fatalf("started = %t, want %t", started, testCase.wantStart)
			}

			if done != nil {
				stopServe()
				<-done
			}

			warned := strings.Contains(stderr.String(), "auto-update not started")
			if warned != testCase.wantWarn {
				t.Errorf("warned = %t, want %t:\n%s", warned, testCase.wantWarn, stderr.String())
			}

			if testCase.wantWarn && !strings.Contains(stderr.String(), noSupervisor) {
				t.Errorf("the warning does not say why:\n%s", stderr.String())
			}
		})
	}
}

// TestAutoUpdateBuildsTheCheckoutAndDrains runs the checkout path through to
// the drain: fast-forward onto the upstream, build a binary stamped with it,
// pull, link, and hand the process to the supervisor. A release is published
// too, and the checkout around the binary wins without the API being asked.
func TestAutoUpdateBuildsTheCheckoutAndDrains(t *testing.T) {
	t.Parallel()
	requireGoBuild(t)

	work, upstream := stageClonePair(t)
	wanted := commitAndPush(t, upstream, "upstream.txt")

	releases := startCountingReleaseServer(t,
		defaultStagedRelease([]byte("an asset the checkout never needs\n"), strings.Repeat("e", 40)))

	run := sourceRun(t, work, staleRevision)
	run.NoRelease = false
	run.APIBase = releases.base
	run.Transport = releases.transport

	var order []string

	steps := recordingSteps(t, &order, "")
	steps.Build = func(ctx context.Context, dir string) error {
		order = append(order, stepBuild)

		return goBuildStamped(ctx, dir, filepath.Join(dir, "bin", updateLinkName))
	}

	drain := api.NewDrain()
	loop := cli.New(&bytes.Buffer{}, &bytes.Buffer{}).NewAutoUpdateLoop(run, drain, steps)

	if !loop.Tick(t.Context()) || !drain.Draining() {
		t.Fatal("the checkout was built without draining the process")
	}

	want := []string{stepBuild, stepPull, stepLink}
	if !slices.Equal(order, want) {
		t.Errorf("step order = %v, want %v", order, want)
	}

	if got := releases.calls.Load(); got != 0 {
		t.Errorf("the release API was called %d time(s) for a binary built from a checkout", got)
	}

	if head := gitOut(t, work, "rev-parse", "HEAD"); head != wanted {
		t.Errorf("HEAD = %s, want the upstream commit %s", head, wanted)
	}
}

// TestAutoUpdateWaitsWhileAnotherUpdateHoldsTheLock covers a manual update in
// flight when a tick comes due. The tick builds nothing and remembers nothing,
// so the next one installs once the lock is free.
func TestAutoUpdateWaitsWhileAnotherUpdateHoldsTheLock(t *testing.T) {
	t.Parallel()
	requireGit(t)

	work, upstream := stageClonePair(t)
	commitAndPush(t, upstream, "upstream.txt")

	run := sourceRun(t, work, staleRevision)

	release, err := cli.HoldUpdateLock(run.Home)
	if err != nil {
		t.Fatalf("HoldUpdateLock() error = %v", err)
	}

	var builds int

	steps := cli.UpdateSteps{
		Build: func(context.Context, string) error {
			builds++

			return errBuildBroken
		},
	}

	loop := cli.New(&bytes.Buffer{}, &bytes.Buffer{}).NewAutoUpdateLoop(run, api.NewDrain(), steps)

	if loop.Tick(t.Context()) || builds != 0 {
		t.Fatalf("the loop built %d time(s) while another update held the lock", builds)
	}

	release()
	loop.Tick(t.Context())

	if builds != 1 {
		t.Fatalf("builds = %d once the lock was free, want the waiting revision tried", builds)
	}
}

// TestAutoUpdateLogsAStepThatFailsAfterTheDrain covers the link failing once
// the drain is asked for. With nothing in flight the drain cancels serve's
// context at once, and the failure must still reach the log.
func TestAutoUpdateLogsAStepThatFailsAfterTheDrain(t *testing.T) {
	t.Parallel()
	requireGoBuild(t)

	asset, revision := buildStampedBinary(t)
	releases := startReleaseServer(t, defaultStagedRelease(asset, revision))

	target := filepath.Join(t.TempDir(), updateLinkName)
	writeFile(t, target, updateOldContent)

	ctx, stopServe := context.WithCancel(t.Context())
	defer stopServe()

	var (
		order  []string
		stderr bytes.Buffer
	)

	steps := recordingSteps(t, &order, target)
	steps.Link = func(string) error {
		// This is what serve's drain watcher does the moment nothing is in flight.
		stopServe()

		return errLinkBroken
	}

	drain := api.NewDrain()
	loop := cli.New(&bytes.Buffer{}, &stderr).NewAutoUpdateLoop(&cli.UpdateRun{
		APIBase:    releases.base,
		Transport:  releases.transport,
		Executable: target,
		Home:       t.TempDir(),
		Running:    staleRevision,
	}, drain, steps)

	if !loop.Tick(ctx) || !drain.Draining() {
		t.Fatal("a process draining onto the new build did not finish the loop")
	}

	for _, want := range []string{"level=ERROR", "step=link", "wanted=" + revision} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("the failure log is missing %q:\n%s", want, stderr.String())
		}
	}
}

// TestAutoUpdateRemembersAFailedRevisionAcrossRestarts covers a serve that dies
// while it installs, which is how running the host out of memory can end. The
// attempt is on disk before the install starts, so the serve systemd starts
// next skips that revision instead of building it again.
func TestAutoUpdateRemembersAFailedRevisionAcrossRestarts(t *testing.T) {
	t.Parallel()
	requireGit(t)

	work, upstream := stageClonePair(t)
	commitAndPush(t, upstream, "upstream.txt")

	var builds int

	steps := cli.UpdateSteps{
		Build: func(context.Context, string) error {
			builds++

			return errBuildBroken
		},
	}

	run := sourceRun(t, work, staleRevision)
	app := cli.New(&bytes.Buffer{}, &bytes.Buffer{})

	app.NewAutoUpdateLoop(run, api.NewDrain(), steps).Tick(t.Context())

	// A new loop over the same home is what the restarted serve builds.
	app.NewAutoUpdateLoop(run, api.NewDrain(), steps).Tick(t.Context())

	if builds != 1 {
		t.Fatalf("builds = %d, want the restarted loop to skip the revision that already failed", builds)
	}
}

// TestAutoUpdateForgetsAnAttemptAShutdownCutShort covers serve stopping in the
// middle of an install. That says nothing about the revision, so nothing is
// logged as a failure and the next serve tries it again.
func TestAutoUpdateForgetsAnAttemptAShutdownCutShort(t *testing.T) {
	t.Parallel()
	requireGit(t)

	work, upstream := stageClonePair(t)
	commitAndPush(t, upstream, "upstream.txt")

	ctx, stopServe := context.WithCancel(t.Context())
	defer stopServe()

	var builds int

	steps := cli.UpdateSteps{
		Build: func(context.Context, string) error {
			builds++

			stopServe()

			return context.Canceled
		},
	}

	var stderr bytes.Buffer

	run := sourceRun(t, work, staleRevision)
	app := cli.New(&bytes.Buffer{}, &stderr)

	if !app.NewAutoUpdateLoop(run, api.NewDrain(), steps).Tick(ctx) {
		t.Fatal("the loop kept going after serve shut down")
	}

	if strings.Contains(stderr.String(), "level=ERROR") {
		t.Errorf("a shutdown was logged as a failed update:\n%s", stderr.String())
	}

	steps.Build = func(context.Context, string) error {
		builds++

		return errBuildBroken
	}

	app.NewAutoUpdateLoop(run, api.NewDrain(), steps).Tick(t.Context())

	if builds != 2 {
		t.Fatalf("builds = %d, want the next serve to try the interrupted revision again", builds)
	}
}

// sourceRun stages a loop invocation on the checkout path: the running binary
// sits in work, so its upstream decides. NoRelease keeps a case off
// api.github.com should checkout detection ever break.
func sourceRun(t *testing.T, work, running string) *cli.UpdateRun {
	t.Helper()

	return &cli.UpdateRun{
		Executable: filepath.Join(work, "bin", updateLinkName),
		Home:       t.TempDir(),
		NoRelease:  true,
		Running:    running,
	}
}
