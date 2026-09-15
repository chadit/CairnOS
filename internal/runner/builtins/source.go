package builtins

import (
	"context"
	"sync"
	"time"

	"github.com/chadit/CairnOS/internal/source"
	"github.com/chadit/CairnOS/pkg/runner"
)

// SourceName is the registry name of the source-sync runner.
const SourceName = "source"

// paramSource names the [source.<name>] block a task syncs.
const paramSource = "source"

// codeSourceUnknown is the one code this runner adds to the set internal/source
// already reports: a task naming a block the host never configured.
const codeSourceUnknown = SourceName + ".unknown"

// metaRef, metaOld, and metaNew report what the sync did, so a workflow can read
// the movement from Meta instead of parsing Output.
const (
	metaRef = "ref"
	metaOld = "old"
	metaNew = "new"
)

// Source brings one configured git tree to the current tip of its ref. Polling
// is an ordinary scheduled workflow rather than a loop inside serve, so this
// runner is the whole of the sync path.
//
// Config: none, since the blocks live in [source.<name>]. Params: source.
type Source struct {
	specs map[string]source.Spec
	locks map[string]*sync.Mutex
	git   source.Git
}

// SourceFactory adapts the configured specs to the registry contract. Every
// instance shares one lock per spec, which is what keeps a webhook fire and a
// schedule fire from running git in the same tree at once (FR12).
func SourceFactory(specs []source.Spec, git source.Git) runner.Factory {
	indexed := make(map[string]source.Spec, len(specs))
	locks := make(map[string]*sync.Mutex, len(specs))

	for _, spec := range specs {
		indexed[spec.Name] = spec
		locks[spec.Name] = &sync.Mutex{}
	}

	return func(_ map[string]any) (runner.Runner, error) {
		return &Source{specs: indexed, locks: locks, git: git}, nil
	}
}

// Name implements runner.Runner.
func (*Source) Name() string {
	return SourceName
}

// Validate implements runner.Runner. A literal name is checked against the
// configured blocks at workflow load, so a typo fails before anything runs; a
// name still carrying a reference waits for the launch-time pass.
func (s *Source) Validate(task *runner.Task) error {
	if err := requireStringParam(task, paramSource, SourceName); err != nil {
		return err
	}

	name, _ := task.Params[paramSource].(string)

	if !task.Resolved && containsRefDelimiter(name) {
		return nil
	}

	if _, known := s.specs[name]; !known {
		return unknownSource(name)
	}

	return nil
}

// unknownSource is the refusal for a name no [source.<name>] block declares.
func unknownSource(name string) error {
	return runner.NewError(runner.ClassPermanent, codeSourceUnknown,
		"no [source."+name+"] block is configured")
}

// Execute implements runner.Runner. Output is empty when the sync moved nothing,
// which is what lets a workflow condition a message on "something changed"
// without inspecting Meta.
func (s *Source) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	if err := s.Validate(task); err != nil {
		return runner.Result{}, err
	}

	name, _ := task.Params[paramSource].(string)

	// Validate lets an unresolved reference through for the launch-time pass, so a
	// direct caller that never resolved it reaches here with a name no block
	// matches. A missing lock is that case, and locking nil would panic.
	lock := s.locks[name]
	if lock == nil {
		return runner.Result{}, unknownSource(name)
	}

	// Latency starts before the lock so a fire that queued behind another shows
	// the wait FR12 creates rather than hiding it.
	start := time.Now()

	lock.Lock()

	defer lock.Unlock()

	spec := s.specs[name]

	result, err := source.Sync(ctx, s.git, spec)
	if err != nil {
		// Sync already returns a classified runner error with the code the engine
		// retries on; wrapping it would hide that class behind a plain error.
		return runner.Result{}, err //nolint:wrapcheck // already a classified runner.Error
	}

	return runner.Result{
		Output: syncOutput(result),
		Usage:  runner.Usage{Latency: time.Since(start)},
		Meta: map[string]string{
			paramSource: name,
			metaRef:     spec.Ref,
			metaOld:     result.Old,
			metaNew:     result.New,
		},
	}, nil
}

// syncOutput renders the movement a workflow reads: the empty string when HEAD
// stayed put, and old..new when it moved.
func syncOutput(result source.Result) string {
	if result.Old == result.New {
		return ""
	}

	return result.Old + ".." + result.New
}
