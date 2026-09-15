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
	gitRevParse = "rev-parse"
	gitStatus   = "status"
	gitMerge    = "merge"
	gitSymRef   = "symbolic-ref"
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
	calls    [][]string
	mu       sync.Mutex
	overlaps atomic.Int32
	inFlight atomic.Int32
}

// argvs returns a copy of every invocation the fake has answered.
func (f *fakeGit) argvs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.calls)
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

	f.calls = append(f.calls, slices.Clone(argv))

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
	case argv[0] == gitMerge:
		f.head = f.afterFF

		return "", nil
	case argv[0] == gitRevParse:
		return f.head, nil
	case argv[0] == gitStatus:
		return "", nil
	default:
		return "", nil
	}
}

// sourceRunner builds the runner under test over one spec and one fake git.
func sourceRunner(t *testing.T, git *fakeGit) runner.Runner {
	t.Helper()

	specs := []source.Spec{{
		Name: sourceSpecName,
		Repo: "https://example.com/automation.git",
		Dest: sourceDest,
		Ref:  sourceRef,
	}}

	built, err := builtins.SourceFactory(specs, git.run)(nil)
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

// TestSourceNeverRunsDestructiveVerbs walks a whole successful sync and reads
// back every invocation the fake answered, so a later "repair a dirty tree" path
// fails here rather than resting on a reading of the code.
func TestSourceNeverRunsDestructiveVerbs(t *testing.T) {
	t.Parallel()

	// The write commands FR8 says a sync never runs. Each one would discard work
	// the tree carries instead of refusing to touch it.
	forbiddenVerbs := []string{"reset", "stash", "clean", "pull", "rebase", "push", "commit"}

	git := &fakeGit{head: headBefore, afterFF: headAfter}

	if _, err := sourceRunner(t, git).Execute(t.Context(), sourceTask(sourceSpecName)); err != nil {
		t.Fatalf("execute: %v", err)
	}

	invocations := git.argvs()
	if len(invocations) == 0 {
		t.Fatal("the sync issued no git invocations at all")
	}

	for _, argv := range invocations {
		if slices.Contains(forbiddenVerbs, argv[0]) {
			t.Fatalf("the sync ran %q; FR8 forbids it", strings.Join(argv, " "))
		}

		if argv[0] == gitMerge && !slices.Contains(argv, "--ff-only") {
			t.Fatalf("the sync ran %q without --ff-only", strings.Join(argv, " "))
		}
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
