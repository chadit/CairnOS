package builtins_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Memory tool fixture values.
const (
	scopeProject = "project:test"
	noteContent  = "the deploy needs VPN access"
	entryID      = "m1"
	queryDeploy  = "deploy"
	tagOps       = "ops"
	queryKey     = "query"
	scopeKey     = "scope"
	opSearch     = "search"
	opAdd        = "add"
	opLoad       = "load"

	// addedEntryID is the id the fake store assigns every added note.
	addedEntryID = "mem-1"
)

var errMemoryDown = errors.New("memory store down")

// fakeMemoryStore records every call so tests can assert scope and
// argument passthrough.
type fakeMemoryStore struct {
	failWith error
	entries  []memory.Entry
	searches []memory.Query
	added    []memory.Entry
	loads    []string
	mu       sync.Mutex
}

var _ memory.Store = (*fakeMemoryStore)(nil)

// Add implements memory.Store.
func (f *fakeMemoryStore) Add(_ context.Context, entry *memory.Entry) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failWith != nil {
		return "", f.failWith
	}

	f.added = append(f.added, *entry)

	return addedEntryID, nil
}

// Get implements memory.Store; the tool never calls it.
func (f *fakeMemoryStore) Get(_ context.Context, _ string) (memory.Entry, error) {
	return memory.Entry{}, f.failWith
}

// Replace implements memory.Store; the tool never calls it.
func (f *fakeMemoryStore) Replace(_ context.Context, _ string, _ *memory.Entry) error {
	return f.failWith
}

// Delete implements memory.Store; the tool never calls it.
func (f *fakeMemoryStore) Delete(_ context.Context, _ string) error {
	return f.failWith
}

// Search implements memory.Store.
func (f *fakeMemoryStore) Search(_ context.Context, query memory.Query) (memory.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failWith != nil {
		return memory.SearchResult{}, f.failWith
	}

	f.searches = append(f.searches, query)

	return memory.SearchResult{Entries: f.entries}, nil
}

// List implements memory.Store; the tool never calls it.
func (f *fakeMemoryStore) List(_ context.Context, _ string) ([]memory.Entry, error) {
	return nil, f.failWith
}

// Load implements memory.Store.
func (f *fakeMemoryStore) Load(_ context.Context, scope string, _ int) ([]memory.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failWith != nil {
		return nil, f.failWith
	}

	f.loads = append(f.loads, scope)

	return f.entries, nil
}

// memoryTask builds a resolved task for the tool.
func memoryTask(params map[string]any) *runner.Task {
	return &runner.Task{RunID: testRunID, ActionName: "brain", Params: params, Resolved: true}
}

func TestMemoryToolSearchScopesAndRendersEntries(t *testing.T) {
	t.Parallel()

	mem := &fakeMemoryStore{entries: []memory.Entry{
		{ID: entryID, Content: noteContent, Importance: 80, Tags: []string{tagOps}},
	}}
	tool := builtins.NewMemoryTool(mem)

	result, err := tool.Execute(t.Context(), memoryTask(map[string]any{
		opKey: opSearch, queryKey: queryDeploy, scopeKey: scopeProject, "limit": float64(3),
	}))
	if err != nil {
		t.Fatalf("execute search: %v", err)
	}

	if len(mem.searches) != 1 || mem.searches[0].Scope != scopeProject ||
		mem.searches[0].Text != queryDeploy || mem.searches[0].Limit != 3 {
		t.Fatalf("store query = %+v, want scope/query/limit passed through", mem.searches)
	}

	output, isMap := result.Output.(map[string]any)
	if !isMap {
		t.Fatalf("output = %#v, want a map", result.Output)
	}

	entries, _ := output["entries"].([]map[string]any)
	if len(entries) != 1 || entries[0]["content"] != noteContent || entries[0]["importance"] != 80 {
		t.Fatalf("entries = %#v, want the rendered store entry", output["entries"])
	}
}

func TestMemoryToolAddBuildsEntry(t *testing.T) {
	t.Parallel()

	mem := &fakeMemoryStore{}
	tool := builtins.NewMemoryTool(mem)

	result, err := tool.Execute(t.Context(), memoryTask(map[string]any{
		opKey:        opAdd,
		"content":    noteContent,
		scopeKey:     scopeProject,
		"importance": float64(70),
		"tags":       []any{tagOps, 42, queryDeploy},
	}))
	if err != nil {
		t.Fatalf("execute add: %v", err)
	}

	if len(mem.added) != 1 {
		t.Fatalf("store added %d entries, want 1", len(mem.added))
	}

	entry := mem.added[0]
	if entry.Scope != scopeProject || entry.Content != noteContent || entry.Importance != 70 {
		t.Fatalf("added entry = %+v, want scope/content/importance from params", entry)
	}

	// Non-string tag elements drop instead of failing the call.
	if !reflect.DeepEqual(entry.Tags, []string{tagOps, queryDeploy}) {
		t.Fatalf("tags = %#v, want the string elements only", entry.Tags)
	}

	output, _ := result.Output.(map[string]any)
	if output["id"] != addedEntryID {
		t.Fatalf("output = %#v, want the new entry id", result.Output)
	}
}

func TestMemoryToolLoadUsesScopeAndBudget(t *testing.T) {
	t.Parallel()

	mem := &fakeMemoryStore{entries: []memory.Entry{{ID: entryID, Content: noteContent}}}
	tool := builtins.NewMemoryTool(mem)

	result, err := tool.Execute(t.Context(), memoryTask(map[string]any{
		opKey: opLoad, scopeKey: scopeProject,
	}))
	if err != nil {
		t.Fatalf("execute load: %v", err)
	}

	if len(mem.loads) != 1 || mem.loads[0] != scopeProject {
		t.Fatalf("store loads = %v, want one load in %q", mem.loads, scopeProject)
	}

	output, _ := result.Output.(map[string]any)

	entries, _ := output["entries"].([]map[string]any)
	if len(entries) != 1 || entries[0]["id"] != entryID {
		t.Fatalf("output = %#v, want the loaded entry", result.Output)
	}
}

func TestMemoryToolValidateRejectsBadParams(t *testing.T) {
	t.Parallel()

	tool := builtins.NewMemoryTool(&fakeMemoryStore{})

	tests := []struct {
		params map[string]any
		name   string
	}{
		{name: caseMissingOp, params: map[string]any{queryKey: "x"}},
		{name: caseUnknownOp, params: map[string]any{opKey: "purge"}},
		{name: "search without query", params: map[string]any{opKey: opSearch}},
		{name: "add without content", params: map[string]any{opKey: opAdd}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tool.Validate(memoryTask(tt.params))

			classified, isClassified := errors.AsType[*runner.Error](err)
			if !isClassified || classified.Class != runner.ClassPermanent {
				t.Fatalf("validate error = %v, want a permanent runner error", err)
			}
		})
	}
}

func TestMemoryToolStoreFailureClassifiesPermanent(t *testing.T) {
	t.Parallel()

	tool := builtins.NewMemoryTool(&fakeMemoryStore{failWith: errMemoryDown})

	_, err := tool.Execute(t.Context(), memoryTask(map[string]any{opKey: opLoad}))

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified || classified.Class != runner.ClassPermanent || !errors.Is(err, errMemoryDown) {
		t.Fatalf("execute error = %v, want a permanent error wrapping the store failure", err)
	}
}

func TestMemoryToolDescribeOmitsScope(t *testing.T) {
	t.Parallel()

	spec := builtins.NewMemoryTool(&fakeMemoryStore{}).Describe()

	if spec.Name != builtins.MemoryName || spec.Description == "" {
		t.Fatalf("spec = %+v, want a named, described tool", spec)
	}

	properties, _ := spec.InputSchema["properties"].(map[string]any)
	if _, exposed := properties[scopeKey]; exposed {
		t.Fatal("schema exposes scope; the loop injects it, drivers must not pick one")
	}

	if _, hasOp := properties[opKey]; !hasOp {
		t.Fatalf("schema properties = %#v, want op present", properties)
	}

	required, _ := spec.InputSchema["required"].([]string)
	if !reflect.DeepEqual(required, []string{opKey}) {
		t.Fatalf("required = %#v, want op only", spec.InputSchema["required"])
	}
}
