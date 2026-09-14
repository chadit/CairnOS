package api

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/workflow"
)

// WorkflowResolver loads a workflow by name out of a directory, freshly on
// every call so edits apply without re-registration. It is the shared run
// lookup the WorkflowService and the A2A adapter both go through, so the two
// resolve names identically (the A2A adapter lives in internal/a2a and reuses
// this exported surface rather than reimplementing the scan).
type WorkflowResolver struct {
	logger *slog.Logger
	dir    string
}

// NewWorkflowResolver builds a resolver over one workflows directory.
func NewWorkflowResolver(dir string, logger *slog.Logger) *WorkflowResolver {
	if logger == nil {
		logger = slog.Default()
	}

	return &WorkflowResolver{dir: dir, logger: logger}
}

// Resolve returns the workflow named name, or a canonical NotFound gRPC error
// when no file in the directory carries that name. Files that fail to load are
// skipped with a log line so one broken file cannot mask the others.
func (r *WorkflowResolver) Resolve(name string) (*workflow.Workflow, error) {
	_, flow, err := r.ResolvePath(name)

	return flow, err
}

// ResolvePath returns the path of the .toml file whose workflow name matches
// name, plus the loaded workflow. The error contract matches Resolve: a
// canonical NotFound when no file carries the name, and unloadable files
// skipped with a log line. Save goes through this so internal/api holds one
// name-to-file scan (.agents/spec/parity/builder-ops.md, "One resolver, no
// third scan").
func (r *WorkflowResolver) ResolvePath(name string) (string, *workflow.Workflow, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return "", nil, errorStatus(codes.NotFound,
			cairnerr.Newf(codeWorkflowNotFound, "no workflow named %q: workflows directory unreadable: %v", name, err))
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), workflowFileSuffix) {
			continue
		}

		path := filepath.Join(r.dir, entry.Name())

		flow, loadErr := workflow.Load(path)
		if loadErr != nil {
			r.logger.Warn("skipping unloadable workflow file", "path", path, "error", loadErr)

			continue
		}

		if flow.Name == name {
			return path, flow, nil
		}
	}

	return "", nil, errorStatus(codes.NotFound,
		cairnerr.Newf(codeWorkflowNotFound, "no workflow named %q in %s", name, r.dir))
}
