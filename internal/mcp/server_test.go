package mcp_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/store"
)

// statusSucceeded is the terminal status a completed mock run lands in.
const statusSucceeded = "succeeded"

// Tool names and the argument keys the tests pass, named so the assertions
// reference the protocol surface rather than scattering string literals.
const (
	toolRunWorkflow      = "run_workflow"
	toolQueryMemory      = "query_memory"
	toolGetLanguageRules = "get_language_rules"
	argWorkflow          = "workflow"
	argText              = "text"
	argLanguage          = "language"
)

// Resource URIs the server must list; the runs resource is also read back.
const (
	workflowsURI = "cairnos://workflows"
	runsURI      = "cairnos://runs"
)

// codeForbidden and codeUnauthenticated are the canonical auth codes a denied
// MCP call carries, the same strings the gRPC interceptor answers with.
const (
	codeForbidden       = "auth.forbidden"
	codeUnauthenticated = "auth.unauthenticated"
)

func TestListToolsDiscoversToolsWithSchemas(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{})
	session := fix.connect(t, "")

	result, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	names := make([]string, 0, len(result.Tools))

	for _, tool := range result.Tools {
		names = append(names, tool.Name)

		if tool.InputSchema == nil {
			t.Errorf("tool %q has no input schema", tool.Name)
		}
	}

	for _, want := range []string{toolRunWorkflow, toolQueryMemory} {
		if !slices.Contains(names, want) {
			t.Errorf("tools/list missing %q; got %v", want, names)
		}
	}
}

func TestRunWorkflowStartsRealRun(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{})
	session := fix.connect(t, "")

	result, err := session.CallTool(t.Context(), &sdk.CallToolParams{
		Name:      toolRunWorkflow,
		Arguments: map[string]any{argWorkflow: helloWorkflow},
	})
	if err != nil {
		t.Fatalf("call run_workflow: %v", err)
	}

	if result.IsError {
		t.Fatalf("run_workflow returned tool error: %s", resultText(result.Content))
	}

	var out struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}

	decodeStructured(t, result.StructuredContent, &out)

	if out.RunID == "" {
		t.Fatal("run_workflow returned empty run id")
	}

	run, err := fix.store.GetRun(t.Context(), out.RunID)
	if err != nil {
		t.Fatalf("GetRun(%q): %v", out.RunID, err)
	}

	if run.WorkflowName != helloWorkflow {
		t.Errorf("run workflow name = %q, want %q", run.WorkflowName, helloWorkflow)
	}

	if string(run.Status) != statusSucceeded {
		t.Errorf("run status = %q, want %q", run.Status, statusSucceeded)
	}
}

// TestRunWorkflowStampsCallerWorkspace proves an MCP run_workflow started with a
// workspace-scoped token lands the run in that token's workspace, not the
// default. A workspace-B-scoped store read sees the run; a workspace-A read does
// not; and the budget spend sums against B, not default. This is the regression
// guard for the run_workflow handler stamping the caller's workspace before
// StartRun.
func TestRunWorkflowStampsCallerWorkspace(t *testing.T) {
	t.Parallel()

	const (
		workspaceB = "tenant-b"
		workspaceA = "tenant-a"
		helloCost  = 0.25
	)

	fix := startFixture(t, fixtureOptions{authEnabled: true})

	token := fix.workspaceToken(t, workspaceB, auth.ScopeWriteWorkflow)
	session := fix.connect(t, token)

	result, err := session.CallTool(t.Context(), &sdk.CallToolParams{
		Name:      toolRunWorkflow,
		Arguments: map[string]any{argWorkflow: helloWorkflow},
	})
	if err != nil {
		t.Fatalf("call run_workflow: %v", err)
	}

	if result.IsError {
		t.Fatalf("run_workflow returned tool error: %s", resultText(result.Content))
	}

	var out struct {
		RunID string `json:"run_id"`
	}

	decodeStructured(t, result.StructuredContent, &out)

	if out.RunID == "" {
		t.Fatal("run_workflow returned empty run id")
	}

	bCtx := store.ContextWithWorkspace(t.Context(), workspaceB)
	aCtx := store.ContextWithWorkspace(t.Context(), workspaceA)

	// Workspace B owns the run.
	run, err := fix.store.GetRun(bCtx, out.RunID)
	if err != nil {
		t.Fatalf("workspace-B GetRun(%q): %v", out.RunID, err)
	}

	if run.WorkspaceID != workspaceB {
		t.Errorf("run workspace = %q, want %q", run.WorkspaceID, workspaceB)
	}

	// Workspace A cannot see the run: it reads as not-found across the boundary.
	if _, getRunErr := fix.store.GetRun(aCtx, out.RunID); !errors.Is(getRunErr, store.ErrRunNotFound) {
		t.Errorf("workspace-A GetRun(%q) error = %v, want ErrRunNotFound", out.RunID, getRunErr)
	}

	// The budget check sums against workspace B, not the default workspace.
	since := time.Now().Add(-time.Hour)

	bSpend, err := fix.store.SpendSince(bCtx, helloWorkflow, since)
	if err != nil {
		t.Fatalf("workspace-B SpendSince: %v", err)
	}

	if bSpend != helloCost {
		t.Errorf("workspace-B spend = %v, want %v", bSpend, helloCost)
	}

	defaultSpend, err := fix.store.SpendSince(t.Context(), helloWorkflow, since)
	if err != nil {
		t.Fatalf("default-workspace SpendSince: %v", err)
	}

	if defaultSpend != 0 {
		t.Errorf("default-workspace spend = %v, want 0; the run leaked into the default budget", defaultSpend)
	}
}

func TestRunWorkflowUnknownWorkflowErrors(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{})
	session := fix.connect(t, "")

	result, err := session.CallTool(t.Context(), &sdk.CallToolParams{
		Name:      toolRunWorkflow,
		Arguments: map[string]any{argWorkflow: "does_not_exist"},
	})
	if err != nil {
		t.Fatalf("call run_workflow: %v", err)
	}

	if !result.IsError {
		t.Fatal("run_workflow with an unknown workflow should be a tool error")
	}
}

func TestQueryMemoryReturnsSeededEntries(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{})
	session := fix.connect(t, "")

	result, err := session.CallTool(t.Context(), &sdk.CallToolParams{
		Name:      toolQueryMemory,
		Arguments: map[string]any{argText: memoryContent, "scope": memoryScope},
	})
	if err != nil {
		t.Fatalf("call query_memory: %v", err)
	}

	if result.IsError {
		t.Fatalf("query_memory returned tool error: %s", resultText(result.Content))
	}

	var out struct {
		Entries []struct {
			Content string `json:"content"`
		} `json:"entries"`
	}

	decodeStructured(t, result.StructuredContent, &out)

	if len(out.Entries) == 0 {
		t.Fatal("query_memory returned no entries")
	}

	if out.Entries[0].Content != memoryContent {
		t.Errorf("entry content = %q, want %q", out.Entries[0].Content, memoryContent)
	}
}

func TestGetLanguageRulesReturnsMergedSet(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{})
	session := fix.connect(t, "")

	result, err := session.CallTool(t.Context(), &sdk.CallToolParams{
		Name:      toolGetLanguageRules,
		Arguments: map[string]any{argLanguage: rulesLang},
	})
	if err != nil {
		t.Fatalf("call get_language_rules: %v", err)
	}

	if result.IsError {
		t.Fatalf("get_language_rules returned tool error: %s", resultText(result.Content))
	}

	var out struct {
		Rules    map[string]string `json:"rules"`
		Language string            `json:"language"`
	}

	decodeStructured(t, result.StructuredContent, &out)

	if out.Language != rulesLang || out.Rules[rulesKey] != rulesValue {
		t.Fatalf("rules = %+v, want language %s with %s=%s", out, rulesLang, rulesKey, rulesValue)
	}
}

func TestListResourcesDiscoversWorkflowsAndRuns(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{})
	session := fix.connect(t, "")

	result, err := session.ListResources(t.Context(), nil)
	if err != nil {
		t.Fatalf("list resources: %v", err)
	}

	uris := make([]string, 0, len(result.Resources))
	for _, resource := range result.Resources {
		uris = append(uris, resource.URI)
	}

	for _, want := range []string{workflowsURI, runsURI} {
		if !slices.Contains(uris, want) {
			t.Errorf("resources/list missing %q; got %v", want, uris)
		}
	}
}

func TestReadWorkflowsResourceListsDeclaredInputs(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{})
	session := fix.connect(t, "")

	result, err := session.ReadResource(t.Context(), &sdk.ReadResourceParams{URI: workflowsURI})
	if err != nil {
		t.Fatalf("read workflows resource: %v", err)
	}

	if len(result.Contents) == 0 {
		t.Fatal("workflows resource returned no contents")
	}

	body := result.Contents[0].Text
	if !strings.Contains(body, helloWorkflow) {
		t.Errorf("workflows resource missing %q; got %s", helloWorkflow, body)
	}
}

func TestReadRunsResourceReflectsStartedRun(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{})
	session := fix.connect(t, "")

	started, err := session.CallTool(t.Context(), &sdk.CallToolParams{
		Name:      toolRunWorkflow,
		Arguments: map[string]any{argWorkflow: helloWorkflow},
	})
	if err != nil {
		t.Fatalf("call run_workflow: %v", err)
	}

	var out struct {
		RunID string `json:"run_id"`
	}

	decodeStructured(t, started.StructuredContent, &out)

	result, err := session.ReadResource(t.Context(), &sdk.ReadResourceParams{URI: runsURI})
	if err != nil {
		t.Fatalf("read runs resource: %v", err)
	}

	if len(result.Contents) == 0 {
		t.Fatal("runs resource returned no contents")
	}

	if !strings.Contains(result.Contents[0].Text, out.RunID) {
		t.Errorf("runs resource missing run id %q; got %s", out.RunID, result.Contents[0].Text)
	}
}

func TestScopeEnforcement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		args        map[string]any
		name        string
		toolName    string
		wantCodeSub string
		scopes      []string
		wantErr     bool
	}{
		{
			name:        "read_memory_token_cannot_run_workflow",
			scopes:      []string{auth.ScopeReadMemory},
			toolName:    toolRunWorkflow,
			args:        map[string]any{argWorkflow: helloWorkflow},
			wantErr:     true,
			wantCodeSub: codeForbidden,
		},
		{
			name:     "write_workflow_token_can_run_workflow",
			scopes:   []string{auth.ScopeWriteWorkflow},
			toolName: toolRunWorkflow,
			args:     map[string]any{argWorkflow: helloWorkflow},
			wantErr:  false,
		},
		{
			name:     "read_memory_token_can_query_memory",
			scopes:   []string{auth.ScopeReadMemory},
			toolName: toolQueryMemory,
			args:     map[string]any{argText: memoryContent},
			wantErr:  false,
		},
		{
			name:     "read_vault_token_can_get_language_rules",
			scopes:   []string{auth.ScopeReadVault},
			toolName: toolGetLanguageRules,
			args:     map[string]any{argLanguage: rulesLang},
			wantErr:  false,
		},
		{
			name:        "read_memory_token_cannot_get_language_rules",
			scopes:      []string{auth.ScopeReadMemory},
			toolName:    toolGetLanguageRules,
			args:        map[string]any{argLanguage: rulesLang},
			wantErr:     true,
			wantCodeSub: codeForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fix := startFixture(t, fixtureOptions{authEnabled: true})

			token := fix.token(t, tt.scopes...)
			session := fix.connect(t, token)

			result, err := session.CallTool(t.Context(), &sdk.CallToolParams{
				Name:      tt.toolName,
				Arguments: tt.args,
			})
			if err != nil {
				t.Fatalf("call %s: %v", tt.toolName, err)
			}

			if result.IsError != tt.wantErr {
				t.Fatalf("%s IsError = %v, want %v (body: %s)",
					tt.toolName, result.IsError, tt.wantErr, resultText(result.Content))
			}

			if tt.wantErr && !strings.Contains(resultText(result.Content), tt.wantCodeSub) {
				t.Errorf("error body %q missing canonical code %q",
					resultText(result.Content), tt.wantCodeSub)
			}
		})
	}
}

func TestMissingTokenIsUnauthenticated(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{authEnabled: true})
	session := fix.connect(t, "")

	result, err := session.CallTool(t.Context(), &sdk.CallToolParams{
		Name:      toolQueryMemory,
		Arguments: map[string]any{argText: memoryContent},
	})
	if err != nil {
		t.Fatalf("call query_memory: %v", err)
	}

	if !result.IsError {
		t.Fatal("a tokenless call with auth enabled should be a tool error")
	}

	if !strings.Contains(resultText(result.Content), codeUnauthenticated) {
		t.Errorf("error body %q missing %q", resultText(result.Content), codeUnauthenticated)
	}
}

func TestAuthDisabledAllowsEveryCall(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{})
	session := fix.connect(t, "")

	result, err := session.CallTool(t.Context(), &sdk.CallToolParams{
		Name:      toolRunWorkflow,
		Arguments: map[string]any{argWorkflow: helloWorkflow},
	})
	if err != nil {
		t.Fatalf("call run_workflow: %v", err)
	}

	if result.IsError {
		t.Fatalf("auth-disabled run_workflow should pass; got error: %s", resultText(result.Content))
	}
}
