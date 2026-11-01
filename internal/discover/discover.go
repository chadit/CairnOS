// Package discover keeps the scheduler in step with the workflow files a
// [source.<name>] checkout carries, so a workflow pushed to the repository
// starts on its schedule after the next sync with no host step, and one
// deleted from it stops.
package discover

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/chadit/CairnOS/internal/poll"
	"github.com/chadit/CairnOS/internal/scheduler"
	"github.com/chadit/CairnOS/internal/source"
	"github.com/chadit/CairnOS/internal/workflow"
)

// CheckSchedulable applies the checks `cairnos schedule add` makes, so the CLI
// and discovery refuse the same files. An error means every fire would fail;
// a non-empty warning means the file parses but its poll source is unknown,
// which fails at fire time and is worth saying while someone is looking.
func CheckSchedulable(flow *workflow.Workflow) (string, error) {
	if flow.Schedule == "" {
		return "", ErrNoSchedule
	}

	if missing := requiredInputsWithoutDefault(flow); len(missing) > 0 {
		return "", fmt.Errorf("%w: %s; give each one a default", ErrUnsuppliedInputs, strings.Join(missing, ", "))
	}

	if err := scheduler.ValidateCron(flow.Schedule); err != nil {
		return "", fmt.Errorf("schedule: %w", err)
	}

	if flow.Poll != nil {
		if _, known := poll.BuiltinSourceInputs(flow.Poll.Source); !known {
			return fmt.Sprintf("unknown poll source %q; the schedule will fail at fire time", flow.Poll.Source), nil
		}
	}

	return "", nil
}

// requiredInputsWithoutDefault lists, sorted, the required inputs with no
// default that a fire of flow never supplies, so the engine rejects every run.
// A plain scheduled fire supplies no inputs, and a poll fire only the keys its
// source emits. A poll source no wiring builds is skipped, since its keys are
// unknown; CheckSchedulable warns about it instead.
func requiredInputsWithoutDefault(flow *workflow.Workflow) []string {
	var supplied []string

	if flow.Poll != nil {
		emitted, known := poll.BuiltinSourceInputs(flow.Poll.Source)
		if !known {
			return nil
		}

		supplied = emitted
	}

	var missing []string

	for _, name := range slices.Sorted(maps.Keys(flow.Inputs)) {
		if flow.Inputs[name].MustBeSupplied() && !slices.Contains(supplied, name) {
			missing = append(missing, name)
		}
	}

	return missing
}

// Tree is the part of one source discovery reads.
type Tree struct {
	Name string

	// Dest is the checkout's absolute path.
	Dest string

	// Ref is the source's configured ref, which picks the tree discovery
	// lists: origin's tip of that branch as last fetched, or the pinned tag.
	Ref string

	// Globs are the source's workflows key; empty turns discovery off.
	Globs []string
}

// Reconcile makes the schedule store match the scheduled workflow files at
// tree's trusted ref (see source.CommittedFiles). It owns only entries under
// Dest matching Globs, adopting one `schedule add` made for the same path. A
// file failing to load or check is logged and keeps its existing entry, so one
// bad push cannot unschedule a working file or block the rest.
func Reconcile(ctx context.Context, store scheduler.Schedules, git source.Git, tree Tree, logger *slog.Logger) error {
	if len(tree.Globs) == 0 {
		return nil
	}

	spec := source.Spec{Name: tree.Name, Dest: tree.Dest, Ref: tree.Ref}

	files, err := source.CommittedFiles(ctx, git, spec, tree.Globs)
	if err != nil {
		return fmt.Errorf("source %s: %w", tree.Name, err)
	}

	run := reconcile{store: store, tree: tree, logger: logger}
	wanted, skipped := run.load(files)

	existing, err := store.List(ctx)
	if err != nil {
		return fmt.Errorf("source %s: list schedules: %w", tree.Name, err)
	}

	owned := run.owned(existing)

	var errs []error

	for _, path := range slices.Sorted(maps.Keys(wanted)) {
		errs = append(errs, run.ensure(ctx, path, wanted[path], owned[path]))
	}

	for _, path := range slices.Sorted(maps.Keys(owned)) {
		if _, keep := wanted[path]; keep || skipped[path] {
			continue
		}

		errs = append(errs, run.removeAll(ctx, owned[path], "workflow unscheduled, file gone or has no schedule key"))
	}

	if joined := errors.Join(errs...); joined != nil {
		return fmt.Errorf("source %s: %w", tree.Name, joined)
	}

	return nil
}

// reconcile carries one pass's collaborators so each step names only what is
// particular to it.
type reconcile struct {
	store  scheduler.Schedules
	logger *slog.Logger
	tree   Tree
}

// load reads every committed match. wanted maps each schedulable file's
// absolute path to its workflow; skipped holds the files that failed, whose
// existing entries stay put. A file that loads but has no schedule key is in
// neither, which is what lets the caller unschedule it.
func (r *reconcile) load(files []string) (map[string]*workflow.Workflow, map[string]bool) {
	wanted := make(map[string]*workflow.Workflow, len(files))
	skipped := make(map[string]bool, len(files))

	for _, rel := range files {
		path := filepath.Join(r.tree.Dest, filepath.FromSlash(rel))

		flow, err := workflow.Load(path)
		if err != nil {
			r.warn("workflow skipped, file does not load", path, err)

			skipped[path] = true

			continue
		}

		warning, err := CheckSchedulable(flow)
		if errors.Is(err, ErrNoSchedule) {
			continue
		}

		if err != nil {
			r.warn("workflow skipped, file cannot be scheduled", path, err)

			skipped[path] = true

			continue
		}

		if warning != "" {
			r.warn("workflow scheduled with a warning", path, warning)
		}

		wanted[path] = flow
	}

	return wanted, skipped
}

// owned groups the entries this source manages: under Dest and matched by
// Globs. Matching the path, not the glob alone, leaves a hand-added schedule
// elsewhere in the checkout alone. Both sides are compared with symlinks
// resolved, and entries are keyed by the path discovery would store, so
// `schedule add` through another spelling of the directory is adopted, not
// duplicated.
func (r *reconcile) owned(existing []scheduler.Schedule) map[string][]scheduler.Schedule {
	owned := make(map[string][]scheduler.Schedule, len(existing))
	root := resolvePath(r.tree.Dest)

	for idx := range existing {
		sched := &existing[idx]

		rel, err := filepath.Rel(root, resolvePath(sched.WorkflowPath))
		if err != nil || !filepath.IsLocal(rel) {
			continue
		}

		if source.MatchesAny(r.tree.Globs, filepath.ToSlash(rel)) {
			path := filepath.Join(r.tree.Dest, rel)
			owned[path] = append(owned[path], *sched)
		}
	}

	return owned
}

// resolvePath resolves symlinks in path. An entry whose file is gone still has
// its directory resolved, so a deleted workflow's entry is recognized and
// removed; a path with neither left is compared as written.
func resolvePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}

	if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		return filepath.Join(dir, filepath.Base(path))
	}

	return filepath.Clean(path)
}

// ensure leaves exactly one entry for path carrying flow's name and cron. The
// first existing entry is kept when it already matches; any further entries for
// the same path are duplicates and go.
func (r *reconcile) ensure(ctx context.Context, path string, flow *workflow.Workflow, existing []scheduler.Schedule) error {
	if len(existing) == 0 {
		return r.add(ctx, path, flow, "workflow scheduled")
	}

	keep, extra := existing[0], existing[1:]

	errs := []error{r.removeAll(ctx, extra, "duplicate workflow schedule removed")}

	if keep.Cron != flow.Schedule || keep.WorkflowName != flow.Name {
		errs = append(errs,
			r.removeAll(ctx, []scheduler.Schedule{keep}, "workflow schedule replaced"),
			r.add(ctx, path, flow, "workflow scheduled"))
	}

	return errors.Join(errs...)
}

// add stores one entry and logs it.
func (r *reconcile) add(ctx context.Context, path string, flow *workflow.Workflow, message string) error {
	_, err := r.store.Add(ctx, &scheduler.Schedule{
		WorkflowPath: path,
		WorkflowName: flow.Name,
		Cron:         flow.Schedule,
	})
	if err != nil {
		return fmt.Errorf("schedule %s: %w", path, err)
	}

	r.info(message, flow.Name, path, flow.Schedule)

	return nil
}

// removeAll deletes each entry by ID, never by name, since a name can be shared
// with a hand-added schedule this source does not own.
func (r *reconcile) removeAll(ctx context.Context, entries []scheduler.Schedule, message string) error {
	var errs []error

	for idx := range entries {
		sched := &entries[idx]
		if err := r.store.Remove(ctx, sched.ID); err != nil {
			errs = append(errs, fmt.Errorf("unschedule %s: %w", sched.WorkflowPath, err))

			continue
		}

		r.info(message, sched.WorkflowName, sched.WorkflowPath, sched.Cron)
	}

	return errors.Join(errs...)
}

// info logs one change with the fields every change line shares.
func (r *reconcile) info(message, name, path, cron string) {
	if r.logger == nil {
		return
	}

	r.logger.Info(message, "source", r.tree.Name, "workflow", name, "path", path, "cron", cron)
}

// warn logs one file discovery passed over, with the reason.
func (r *reconcile) warn(message, path string, reason any) {
	if r.logger == nil {
		return
	}

	r.logger.Warn(message, "source", r.tree.Name, "path", path, "reason", reason)
}
