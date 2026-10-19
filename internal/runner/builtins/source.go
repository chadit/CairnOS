package builtins

import (
	"context"
	"log/slog"
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
// the movement from Meta instead of parsing Output. metaSkipped appears only on
// a sync that left the tree alone, carrying the reason.
const (
	metaRef     = "ref"
	metaOld     = "old"
	metaNew     = "new"
	metaSkipped = "skipped"
)

// SourceObserver watches sync outcomes that deserve more than a log line.
// internal/notify transitively depends on this package, so a fan-out cannot be
// imported here; internal/cli closes over one and passes it in.
type SourceObserver interface {
	// Skipped reports a tree left alone because dirty_reset is off. The
	// implementation owns any rate limiting: this fires on every skipped sync.
	Skipped(ctx context.Context, name, dest, reason string)

	// Synced reports a sync that was not skipped, which is what lets a rate
	// limiter forget a source whose tree has been put right.
	Synced(name string)
}

// Source brings one configured git tree to the current tip of its ref. Polling
// is an ordinary scheduled workflow rather than a loop inside serve, so this
// runner is the whole of the sync path.
//
// Config: none, since the blocks live in [source.<name>]. Params: source.
type Source struct {
	specs    map[string]source.Spec
	locks    map[string]*sync.Mutex
	git      source.Git
	logger   *slog.Logger
	observer SourceObserver
}

// SourceFactory adapts the configured specs to the registry contract. Every
// instance shares one lock per spec, which is what keeps a webhook fire and a
// schedule fire from running git in the same tree at once (FR12).
//
// logger and observer may both be nil: only serve has a fan-out, and the four
// one-shot commands build the same registry without one.
func SourceFactory(
	specs []source.Spec, git source.Git, logger *slog.Logger, observer SourceObserver,
) runner.Factory {
	indexed := make(map[string]source.Spec, len(specs))
	locks := make(map[string]*sync.Mutex, len(specs))

	for _, spec := range specs {
		indexed[spec.Name] = spec
		locks[spec.Name] = &sync.Mutex{}
	}

	return func(_ map[string]any) (runner.Runner, error) {
		return &Source{specs: indexed, locks: locks, git: git, logger: logger, observer: observer}, nil
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
	return taskError(runner.ClassPermanent, codeSourceUnknown,
		"no [source.%s] block is configured", name)
}

// Execute implements runner.Runner. Output is empty when the sync moved nothing,
// which is what lets a workflow condition a message on "something changed"
// without inspecting Meta. Latency starts before the lock, so a fire that queued
// behind another shows the wait FR12 creates rather than hiding it.
func (s *Source) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	if err := s.Validate(task); err != nil {
		return runner.Result{}, err
	}

	name, _ := task.Params[paramSource].(string)

	lock, err := s.lockFor(name)
	if err != nil {
		return runner.Result{}, err
	}

	start := time.Now()

	lock.Lock()

	defer lock.Unlock()

	spec := s.specs[name]

	result, err := source.Sync(ctx, s.git, spec)
	if err != nil {
		return runner.Result{}, err //nolint:wrapcheck // already a classified runner.Error
	}

	s.report(ctx, spec, result)

	meta := map[string]string{
		paramSource: name,
		metaRef:     spec.Ref,
		metaOld:     result.Old,
		metaNew:     result.New,
	}

	if result.Skipped != "" {
		meta[metaSkipped] = result.Skipped
	}

	return runner.Result{
		Output: syncOutput(result),
		Usage:  runner.Usage{Latency: time.Since(start)},
		Meta:   meta,
	}, nil
}

// lockFor returns the mutex guarding one source. Validate lets an unresolved
// reference through for the launch-time pass, so a direct caller that never
// resolved it arrives with a name no block matches and no lock to take.
func (s *Source) lockFor(name string) (*sync.Mutex, error) {
	lock := s.locks[name]
	if lock == nil {
		return nil, unknownSource(name)
	}

	return lock, nil
}

// report logs what the sync decided and tells the observer. A reset and a skip
// both carry a warn line every time they happen: the log is the record an
// operator can go back to, while the notification is rate limited elsewhere.
func (s *Source) report(ctx context.Context, spec source.Spec, result source.Result) {
	if result.Skipped != "" {
		s.warn("source sync skipped, tree left alone", spec, "reason", result.Skipped)

		if s.observer != nil {
			s.observer.Skipped(ctx, spec.Name, spec.Dest, result.Skipped)
		}

		return
	}

	if result.Reset {
		s.warn("source reset onto ref, local changes discarded", spec, "old", result.Old, "new", result.New)
	}

	if s.observer != nil {
		s.observer.Synced(spec.Name)
	}
}

// warn emits one line with the source fields every log site here shares, so a
// caller states only what is particular to its outcome.
func (s *Source) warn(message string, spec source.Spec, extra ...any) {
	if s.logger == nil {
		return
	}

	args := append([]any{"source", spec.Name, "dest", spec.Dest, "ref", spec.Ref}, extra...)

	s.logger.Warn(message, args...)
}

// syncOutput renders the movement a workflow reads: the empty string when HEAD
// stayed put or the sync was skipped, and old..new when it moved. A skipped sync
// has to answer empty so a workflow conditioned on movement stays quiet.
func syncOutput(result source.Result) string {
	if result.Old == result.New {
		return ""
	}

	return result.Old + ".." + result.New
}
