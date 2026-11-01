package mcp

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/store"
)

// runWorkflowInput is the run_workflow tool's argument shape. The SDK infers the
// tool's JSON schema from this struct and its jsonschema tags, so the schema has
// one source. Inputs is free-form: each workflow file declares its own input
// contract and the engine validates supplied values against it on RunWorkflow,
// so repeating that schema here would be a second, drifting source.
type runWorkflowInput struct {
	Inputs   map[string]any `json:"inputs,omitempty" jsonschema:"the workflow's declared inputs as a JSON object"`
	Workflow string         `json:"workflow"         jsonschema:"the name of the workflow to run"`
}

// runWorkflowOutput is what run_workflow returns: the created run id and its
// status at creation. Progress is not streamed here; a consumer streams
// TaskUpdates over A2A or the gRPC run stream.
type runWorkflowOutput struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

// queryMemoryInput mirrors memory.Query so the generated schema and the store
// query come from the one shape. Limit and Semantic are optional; an absent
// limit lets the backend apply its default.
type queryMemoryInput struct {
	Scope    string   `json:"scope,omitempty"    jsonschema:"narrow the search to one memory scope (e.g. global or project:x)"`
	Text     string   `json:"text"               jsonschema:"the text to search for"`
	Tags     []string `json:"tags,omitempty"     jsonschema:"narrow the search to entries carrying all these tags"`
	Limit    int      `json:"limit,omitempty"    jsonschema:"cap the number of results"`
	Semantic bool     `json:"semantic,omitempty" jsonschema:"use vector search when a vector backend is configured"`
}

// memoryHit is one entry in the query_memory result. It flattens memory.Entry
// to the fields a caller acts on, leaving the internal token-cost accounting
// out of the protocol surface.
type memoryHit struct {
	ID         string   `json:"id"`
	Scope      string   `json:"scope"`
	Content    string   `json:"content"`
	Tags       []string `json:"tags,omitempty"`
	Importance int      `json:"importance"`
}

// queryMemoryOutput carries the hits plus the fallback signal memory.Search
// reports, so a caller that asked for semantic search learns when it got
// keyword results instead.
type queryMemoryOutput struct {
	Entries  []memoryHit `json:"entries"`
	Fallback bool        `json:"fallback"`
}

// Why wait rather than return "running" the instant the engine accepts the run:
// StartRun unblocks the moment the engine consumes the reserved id, which is a
// hair before CreateRun finishes persisting the record (the same race the A2A
// CreateTask comment calls out). Returning then would hand back an id that a
// GetRun could miss. v1 is non-streaming, so waiting for completion is the
// honest answer; a consumer that wants live progress streams TaskUpdates over
// A2A or the gRPC run stream instead.

// runWorkflowHandler returns the typed handler for the run_workflow tool, bound
// to one connection's token. It enforces write:workflow, resolves the named
// workflow, starts a run through the shared manager, and waits for the run to
// finish before answering, so the returned id is a durable record a caller can
// GetRun right away and the status is the real terminal one.
func (s *Service) runWorkflowHandler(token string) mcp.ToolHandlerFor[runWorkflowInput, runWorkflowOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input runWorkflowInput) (*mcp.CallToolResult, runWorkflowOutput, error) {
		workspace, err := s.authorize(ctx, token, auth.ScopeWriteWorkflow)
		if err != nil {
			return nil, runWorkflowOutput{}, err
		}

		flow, err := s.resolver.Resolve(input.Workflow)
		if err != nil {
			return nil, runWorkflowOutput{}, fmt.Errorf("resolve workflow %q: %w", input.Workflow, err)
		}

		// Stamp the caller's workspace so the run lands in their tenant and the
		// budget check sums against their spend, not the default workspace.
		// awaitRun keeps the raw ctx since it only watches for cancellation.
		handle, err := s.manager.StartRun(store.ContextWithWorkspace(ctx, workspace), flow, input.Inputs, false, triggerMCP)
		if err != nil {
			return nil, runWorkflowOutput{}, fmt.Errorf("start run: %w", err)
		}

		return s.awaitRun(ctx, handle)
	}
}

// awaitRun blocks until the run finishes or the caller's context cancels,
// returning the terminal run id and status. A canceled context detaches the
// run, which keeps executing in the background (StartRun ran it under a context
// that does not derive from ctx), so the run is not lost; the caller reads the
// id later through the runs resource.
func (*Service) awaitRun(ctx context.Context, handle *api.RunHandle) (*mcp.CallToolResult, runWorkflowOutput, error) {
	select {
	case <-ctx.Done():
		return nil, runWorkflowOutput{RunID: handle.ID, Status: string(store.RunRunning)}, nil
	case outcome := <-handle.Outcome:
		if outcome.Err != nil {
			return nil, runWorkflowOutput{}, fmt.Errorf("run %s: %w", handle.ID, outcome.Err)
		}

		return nil, runWorkflowOutput{RunID: outcome.Run.ID, Status: string(outcome.Run.Status)}, nil
	}
}

// queryMemoryHandler returns the typed handler for the query_memory tool, bound
// to one connection's token. It enforces read:memory and runs the store search
// under the caller's workspace, so a token scoped to one tenant never reads
// another's entries.
func (s *Service) queryMemoryHandler(token string) mcp.ToolHandlerFor[queryMemoryInput, queryMemoryOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input queryMemoryInput) (*mcp.CallToolResult, queryMemoryOutput, error) {
		workspace, err := s.authorize(ctx, token, auth.ScopeReadMemory)
		if err != nil {
			return nil, queryMemoryOutput{}, err
		}

		result, err := s.memory.Search(store.ContextWithWorkspace(ctx, workspace), memory.Query{
			Scope:    input.Scope,
			Text:     input.Text,
			Tags:     input.Tags,
			Limit:    input.Limit,
			Semantic: input.Semantic,
		})
		if err != nil {
			return nil, queryMemoryOutput{}, fmt.Errorf("search memory: %w", err)
		}

		return nil, queryMemoryOutput{Entries: memoryHits(result.Entries), Fallback: result.Fallback}, nil
	}
}

// RulesLookup is the narrow slice of the Language Rules Service the MCP server
// needs: one merge call returning a flat map. The serve wiring adapts
// *rules.Service to it, so this package depends on the seam, not the rules
// store package (interfacebloat: compose narrow interfaces).
type RulesLookup interface {
	// GetRules returns the merged rule set for a language, optionally narrowed
	// to a project, as a flat key/value map.
	GetRules(ctx context.Context, language, project string) (map[string]string, error)
}

// getLanguageRulesInput is the get_language_rules tool's argument shape; the
// SDK infers the JSON input schema from it. Project is optional: an absent
// project returns the language defaults.
type getLanguageRulesInput struct {
	Language string `json:"language"          jsonschema:"the language to look up rules for, e.g. go or python"`
	Project  string `json:"project,omitempty" jsonschema:"optional project whose overrides merge over the language defaults"`
}

// getLanguageRulesOutput carries the merged rule set as a flat key/value map,
// the same shape the language_rules_service runner emits.
type getLanguageRulesOutput struct {
	Rules    map[string]string `json:"rules"`
	Language string            `json:"language"`
	Project  string            `json:"project,omitempty"`
}

// getLanguageRulesHandler returns the typed handler for the get_language_rules
// tool, bound to one connection's token. It enforces read:vault (the rules
// service is a typed view over the vault, so its reads sit under the vault read
// scope, matching the gRPC method) and runs the merge under the caller's
// workspace.
func (s *Service) getLanguageRulesHandler(token string) mcp.ToolHandlerFor[getLanguageRulesInput, getLanguageRulesOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input getLanguageRulesInput) (*mcp.CallToolResult, getLanguageRulesOutput, error) {
		workspace, err := s.authorize(ctx, token, auth.ScopeReadVault)
		if err != nil {
			return nil, getLanguageRulesOutput{}, err
		}

		merged, err := s.rules.GetRules(store.ContextWithWorkspace(ctx, workspace), input.Language, input.Project)
		if err != nil {
			return nil, getLanguageRulesOutput{}, fmt.Errorf("get language rules: %w", err)
		}

		return nil, getLanguageRulesOutput{Language: input.Language, Project: input.Project, Rules: merged}, nil
	}
}

// memoryHits flattens store entries to the protocol shape. The loop indexes
// rather than ranging by value because memory.Entry is wide enough that a copy
// per iteration is wasted work (gocritic rangeValCopy).
func memoryHits(entries []memory.Entry) []memoryHit {
	hits := make([]memoryHit, 0, len(entries))

	for idx := range entries {
		entry := &entries[idx]
		hits = append(hits, memoryHit{
			ID:         entry.ID,
			Scope:      entry.Scope,
			Content:    entry.Content,
			Importance: entry.Importance,
			Tags:       entry.Tags,
		})
	}

	return hits
}
