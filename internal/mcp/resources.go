package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
)

// mimeJSON is the content type both resources serve. The SDK carries it on the
// Resource declaration and on each ResourceContents block.
const mimeJSON = "application/json"

// workflowFileSuffix is the extension the workflows resource lists, matching
// the resolver's own scan so the two agree on what counts as a workflow file.
const workflowFileSuffix = ".toml"

// runsResourceLimit caps how many recent runs the runs resource returns, so a
// long-lived node's history does not balloon one resource read.
const runsResourceLimit = 50

// workflowSummary is one entry in the workflows resource: the workflow's name,
// version, description, and its declared inputs, which is exactly what an MCP
// client needs to call run_workflow correctly.
type workflowSummary struct {
	Name        string                  `json:"name"`
	Version     string                  `json:"version,omitempty"`
	Description string                  `json:"description,omitempty"`
	Inputs      map[string]inputSummary `json:"inputs,omitempty"`
}

// inputSummary describes one declared input so a caller can build a valid
// inputs object for run_workflow.
type inputSummary struct {
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Default     any    `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
}

// runSummary is one entry in the runs resource: the run state an external
// client polls without needing the full action transcript.
type runSummary struct {
	ID           string    `json:"id"`
	WorkflowName string    `json:"workflow_name"`
	Status       string    `json:"status"`
	Trigger      string    `json:"trigger"`
	CreatedAt    time.Time `json:"created_at"`
	FinishedAt   time.Time `json:"finished_at,omitzero"`
}

// workflowsResource returns the resource handler for the workflows list, bound
// to one connection's token. It enforces read:workflow and reads each file in
// the configured directory through workflow.Load, the same loader the resolver
// uses, so the declared inputs an MCP client sees match what run_workflow
// validates against. A file that fails to load is skipped rather than failing
// the whole read, so one broken file cannot hide the rest.
func (s *Service) workflowsResource(token string) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		if _, err := s.authorize(ctx, token, auth.ScopeReadWorkflow); err != nil {
			return nil, err
		}

		summaries, err := s.listWorkflows()
		if err != nil {
			return nil, err
		}

		return jsonResource(req.Params.URI, summaries)
	}
}

// runsResource returns the resource handler for the recent-runs list, bound to
// one connection's token. It enforces read:run and reads the store under the
// caller's workspace, so a tenant-scoped token sees only its own runs.
func (s *Service) runsResource(token string) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		workspace, err := s.authorize(ctx, token, auth.ScopeReadRun)
		if err != nil {
			return nil, err
		}

		runs, err := s.store.ListRuns(s.scopedContext(ctx, workspace), store.ListFilter{Limit: runsResourceLimit})
		if err != nil {
			return nil, fmt.Errorf("list runs: %w", err)
		}

		return jsonResource(req.Params.URI, runSummaries(runs))
	}
}

// listWorkflows scans the workflows directory and loads each file into a
// summary. An unreadable directory is an error; an unloadable single file is
// skipped with a log line.
func (s *Service) listWorkflows() ([]workflowSummary, error) {
	entries, err := os.ReadDir(s.workflowsDir)
	if err != nil {
		return nil, fmt.Errorf("read workflows directory: %w", err)
	}

	summaries := make([]workflowSummary, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), workflowFileSuffix) {
			continue
		}

		path := filepath.Join(s.workflowsDir, entry.Name())

		flow, loadErr := workflow.Load(path)
		if loadErr != nil {
			s.logger.Warn("skipping unloadable workflow file", "path", path, "error", loadErr)

			continue
		}

		summaries = append(summaries, summarizeWorkflow(flow))
	}

	return summaries, nil
}

// summarizeWorkflow flattens a loaded workflow to its protocol summary.
func summarizeWorkflow(flow *workflow.Workflow) workflowSummary {
	inputs := make(map[string]inputSummary, len(flow.Inputs))

	for name, decl := range flow.Inputs {
		inputs[name] = inputSummary{
			Type:        decl.Type,
			Required:    decl.Required,
			Default:     decl.Default,
			Description: decl.Description,
		}
	}

	return workflowSummary{
		Name:        flow.Name,
		Version:     flow.Version,
		Description: flow.Description,
		Inputs:      inputs,
	}
}

// runSummaries flattens store runs to their protocol summaries.
func runSummaries(runs []*store.Run) []runSummary {
	summaries := make([]runSummary, 0, len(runs))

	for _, run := range runs {
		summaries = append(summaries, runSummary{
			ID:           run.ID,
			WorkflowName: run.WorkflowName,
			Status:       string(run.Status),
			Trigger:      run.Trigger,
			CreatedAt:    run.CreatedAt,
			FinishedAt:   run.FinishedAt,
		})
	}

	return summaries
}

// jsonResource marshals a value into a single JSON resource content block under
// the requested URI, the one shape both resources answer with.
func jsonResource(uri string, value any) (*mcp.ReadResourceResult, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode resource: %w", err)
	}

	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      uri,
			MIMEType: mimeJSON,
			Text:     string(encoded),
		}},
	}, nil
}

// scopedContext stamps the caller's workspace onto the context so the store
// queries a handler runs filter by the right tenant, the same property the gRPC
// guard gives every handler (.agents/spec/12-ui-and-governance.md,
// "Multi-tenancy").
func (*Service) scopedContext(ctx context.Context, workspace string) context.Context {
	return store.ContextWithWorkspace(ctx, workspace)
}
