package builtins_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/source"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Fixture values every source case addresses: the one configured block, where
// its tree lives, the ref it follows, and the two commits the fake git reports.
const (
	sourceSpecName = "automation"
	sourceDest     = "/opt/src/automation"
	sourceRef      = "main"
	sourceParam    = "source"
	syncAction     = "sync"
	headBefore     = "aaaaaaa"
	headAfter      = "bbbbbbb"
)

// Git subcommands the fake answers by name.
const (
	gitRevParse  = "rev-parse"
	gitStatus    = "status"
	gitMerge     = "merge"
	gitSymRef    = "symbolic-ref"
	gitCheckout  = "checkout"
	dirtyStatus  = " M README.md"
	dirtyMessage = "uncommitted changes"
)

// errNoSuchRef is what the fake reports for a ref that does not resolve, the
// shape git's own --verify failure takes.
var errNoSuchRef = errors.New("fatal: needed a single revision")

// fakeGit answers the fixed command sequence source.Sync walks, so a runner case
// exercises the runner rather than git. head is what rev-parse reports and moves
// to afterFF once the merge runs, which is how a case says whether the sync found
// anything.
type fakeGit struct {
	head     string
	afterFF  string
	status   string
	mu       sync.Mutex
	overlaps atomic.Int32
	inFlight atomic.Int32
}

// run answers one git invocation and records whether a second call overlapped a
// first, which is the only way a case can see the per-spec lock doing its job.
func (f *fakeGit) run(_ context.Context, _ string, argv ...string) (string, error) {
	if f.inFlight.Add(1) > 1 {
		f.overlaps.Add(1)
	}

	defer f.inFlight.Add(-1)

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.answer(argv)
}

// answer maps one argv to the output source.Sync expects from it. The tag lookup
// fails the way git's own --verify does for a name that is not a tag, so every
// case here resolves the ref as a branch.
func (f *fakeGit) answer(argv []string) (string, error) {
	last := argv[len(argv)-1]

	switch {
	case argv[0] == gitRevParse && last == "refs/tags/"+sourceRef:
		return "", errNoSuchRef
	case argv[0] == gitSymRef:
		return sourceRef, nil
	case argv[0] == gitMerge, argv[0] == gitCheckout:
		f.head = f.afterFF

		return "", nil
	case argv[0] == gitRevParse:
		return f.head, nil
	case argv[0] == gitStatus:
		return f.status, nil
	default:
		return "", nil
	}
}

// syncObserver stands in for the notifier serve wires behind the runner, so a
// case can read back which outcome the runner reported rather than watching a
// chat channel.
type syncObserver struct {
	skips  []string
	synced []string
	mu     sync.Mutex
}

// Skipped records the whole line's worth of detail, so a case can prove the dest
// and the reason reach the notifier that builds the operator's message.
func (o *syncObserver) Skipped(_ context.Context, name, dest, reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.skips = append(o.skips, strings.Join([]string{name, dest, reason}, " "))
}

// Synced records a sync that was not skipped, which is the signal that clears
// the notifier's cooldown.
func (o *syncObserver) Synced(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.synced = append(o.synced, name)
}

// seen returns copies of the skip lines and the synced names, read under the
// lock the concurrent case needs.
func (o *syncObserver) seen() ([]string, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	return slices.Clone(o.skips), slices.Clone(o.synced)
}

// sourceSpec is the one configured block every case syncs.
func sourceSpec(dirtyReset bool) source.Spec {
	return source.Spec{
		Name:       sourceSpecName,
		Repo:       "https://example.com/automation.git",
		Dest:       sourceDest,
		Ref:        sourceRef,
		DirtyReset: dirtyReset,
	}
}

// sourceRunner builds the runner under test over one spec and one fake git, with
// no observer: that is what the four one-shot commands wire, and the cases about
// that seam build their own.
func sourceRunner(t *testing.T, git *fakeGit) runner.Runner {
	t.Helper()

	return sourceRunnerWith(t, git, sourceSpec(true), nil)
}

// sourceRunnerWith builds the runner over one spec with the observer a case
// wants to read back. The logger stays nil throughout: warn lines are for an
// operator reading a journal, not something a case asserts on.
func sourceRunnerWith(
	t *testing.T, git *fakeGit, spec source.Spec, obs builtins.SourceObserver,
) runner.Runner {
	t.Helper()

	built, err := builtins.SourceFactory([]source.Spec{spec}, git.run, nil, obs)(nil)
	if err != nil {
		t.Fatalf("build source runner: %v", err)
	}

	return built
}

// sourceTask builds a resolved task naming one source.
func sourceTask(name string) *runner.Task {
	return &runner.Task{
		Params:     map[string]any{sourceParam: name},
		Resolved:   true,
		ActionName: syncAction,
	}
}

func TestSourceValidateRejectsUnknownName(t *testing.T) {
	t.Parallel()

	err := sourceRunner(t, &fakeGit{}).Validate(sourceTask("never_configured"))

	classified, ok := errors.AsType[*runner.Error](err)
	if !ok {
		t.Fatalf("error %v is not a classified runner error", err)
	}

	if classified.Code != builtins.SourceName+".unknown" {
		t.Fatalf("code = %q, want %s.unknown", classified.Code, builtins.SourceName)
	}

	if !strings.Contains(classified.Message, "never_configured") {
		t.Fatalf("message %q does not name the source the workflow asked for", classified.Message)
	}
}

// TestSourceValidateDefersUnresolvedReference proves a param still carrying a
// reference is left to the launch-time pass rather than failing workflow load.
func TestSourceValidateDefersUnresolvedReference(t *testing.T) {
	t.Parallel()

	task := &runner.Task{
		Params:     map[string]any{sourceParam: "${{ inputs.which }}"},
		ActionName: syncAction,
	}

	if err := sourceRunner(t, &fakeGit{}).Validate(task); err != nil {
		t.Fatalf("validate of an unresolved reference = %v, want nil", err)
	}
}

func TestSourceReportsMovementAsOldDotDotNew(t *testing.T) {
	t.Parallel()

	git := &fakeGit{head: headBefore, afterFF: headAfter}

	result, err := sourceRunner(t, git).Execute(t.Context(), sourceTask(sourceSpecName))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.Output != headBefore+".."+headAfter {
		t.Fatalf("output = %q, want %s..%s", result.Output, headBefore, headAfter)
	}

	wantMeta := map[string]string{
		sourceParam: sourceSpecName,
		"ref":       sourceRef,
		"old":       headBefore,
		"new":       headAfter,
	}

	for key, want := range wantMeta {
		if result.Meta[key] != want {
			t.Fatalf("meta[%q] = %q, want %q", key, result.Meta[key], want)
		}
	}
}

func TestSourceReportsEmptyOutputWhenNothingMoved(t *testing.T) {
	t.Parallel()

	git := &fakeGit{head: headBefore, afterFF: headBefore}

	result, err := sourceRunner(t, git).Execute(t.Context(), sourceTask(sourceSpecName))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.Output != "" {
		t.Fatalf("output = %q, want the empty string when HEAD stayed put", result.Output)
	}

	if result.Meta["old"] != result.Meta["new"] {
		t.Fatalf("meta reports movement %q..%q with nothing pushed", result.Meta["old"], result.Meta["new"])
	}
}

// TestSourceReportsSkipAsSuccess covers the mapping FR5 asks of the runner: a
// tree left alone is not a failed run, so the action succeeds with empty output,
// the reason rides in Meta, and the observer hears about it once.
func TestSourceReportsSkipAsSuccess(t *testing.T) {
	t.Parallel()

	git := &fakeGit{head: headBefore, afterFF: headAfter, status: dirtyStatus}
	obs := &syncObserver{}

	result, err := sourceRunnerWith(t, git, sourceSpec(false), obs).
		Execute(t.Context(), sourceTask(sourceSpecName))
	if err != nil {
		t.Fatalf("execute of a skipped sync = %v, want nil", err)
	}

	if result.Output != "" {
		t.Fatalf("output = %q, want empty so a workflow conditioned on movement stays quiet", result.Output)
	}

	if result.Meta["skipped"] != dirtyMessage {
		t.Fatalf("meta[skipped] = %q, want %q", result.Meta["skipped"], dirtyMessage)
	}

	skips, synced := obs.seen()

	if len(synced) != 0 {
		t.Fatalf("observer saw %v synced; a skip must not clear the cooldown", synced)
	}

	want := []string{strings.Join([]string{sourceSpecName, sourceDest, dirtyMessage}, " ")}
	if !slices.Equal(skips, want) {
		t.Fatalf("observer saw %v, want %v", skips, want)
	}
}

// TestSourceReportsResetAsMovement covers FR4 and FR8 at the runner: a repaired
// tree reads as an ordinary move to a workflow, and the observer hears the
// source is healthy again so a pending cooldown clears.
func TestSourceReportsResetAsMovement(t *testing.T) {
	t.Parallel()

	git := &fakeGit{head: headBefore, afterFF: headAfter, status: dirtyStatus}
	obs := &syncObserver{}

	result, err := sourceRunnerWith(t, git, sourceSpec(true), obs).
		Execute(t.Context(), sourceTask(sourceSpecName))
	if err != nil {
		t.Fatalf("execute of a reset sync = %v, want nil", err)
	}

	if result.Output != headBefore+".."+headAfter {
		t.Fatalf("output = %q, want %s..%s", result.Output, headBefore, headAfter)
	}

	if reason, ok := result.Meta["skipped"]; ok {
		t.Fatalf("meta[skipped] = %q on a reset, want the key absent", reason)
	}

	skips, synced := obs.seen()
	if len(skips) != 0 || !slices.Equal(synced, []string{sourceSpecName}) {
		t.Fatalf("observer saw skips=%v synced=%v, want one sync and no skip", skips, synced)
	}
}

// TestSourceSerializesConcurrentSyncs covers FR12: two tasks for one source in
// one process never have git running in that tree at the same time.
func TestSourceSerializesConcurrentSyncs(t *testing.T) {
	t.Parallel()

	git := &fakeGit{head: headBefore, afterFF: headBefore}
	syncer := sourceRunner(t, git)

	const callers = 8

	var group sync.WaitGroup

	for range callers {
		group.Go(func() {
			if _, err := syncer.Execute(t.Context(), sourceTask(sourceSpecName)); err != nil {
				t.Errorf("execute: %v", err)
			}
		})
	}

	group.Wait()

	if overlaps := git.overlaps.Load(); overlaps != 0 {
		t.Fatalf("%d git calls overlapped; the per-source lock did not hold", overlaps)
	}
}

// TestSourceExecuteRefusesUnresolvedName guards the one path Validate lets
// through: a caller that never resolved the reference reaches Execute with a name
// no block declares, where the lock lookup would otherwise answer nil.
func TestSourceExecuteRefusesUnresolvedName(t *testing.T) {
	t.Parallel()

	task := &runner.Task{
		Params:     map[string]any{sourceParam: "${{ inputs.which }}"},
		ActionName: syncAction,
	}

	_, err := sourceRunner(t, &fakeGit{}).Execute(t.Context(), task)

	classified, ok := errors.AsType[*runner.Error](err)
	if !ok {
		t.Fatalf("error %v is not a classified runner error", err)
	}

	if classified.Code != builtins.SourceName+".unknown" {
		t.Fatalf("code = %q, want %s.unknown", classified.Code, builtins.SourceName)
	}
}
