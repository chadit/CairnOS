package builtins

import (
	"context"
	"time"

	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/pkg/runner"
)

// MemoryName is the registry name of the memory built-in tool.
const MemoryName = "memory"

// Memory tool params; paramOp and paramLimit are shared with the github
// runner. paramScope is injected by the agent loop from the action's
// memory_scope, which is why it never appears in the Describe schema: a
// driver does not pick its own scope.
const (
	paramScope      = "scope"
	paramQuery      = "query"
	paramContent    = "content"
	paramImportance = "importance"
	paramTags       = "tags"
	paramBudget     = "token_budget"
)

// Memory ops.
const (
	memOpSearch = "search"
	memOpAdd    = "add"
	memOpLoad   = "load"
)

// Defaults for the optional knobs.
const (
	defaultMemorySearchLimit = 10
	defaultMemoryLoadBudget  = 2048
)

// badOpMessage is the shared complaint for an unknown op value.
const badOpMessage = `op must be "search", "add", or "load"`

// JSON Schema keywords the built-in Describe schemas repeat.
const (
	schemaType       = "type"
	schemaObject     = "object"
	schemaString     = "string"
	schemaInteger    = "integer"
	schemaArray      = "array"
	schemaItems      = "items"
	schemaDesc       = "description"
	schemaProperties = "properties"
	schemaRequired   = "required"
)

// Object keys shared across the built-in tools' outputs and the wire messages
// they send.
const (
	keyEntries  = "entries"
	keyFallback = "fallback"
	keyID       = "id"
	keyName     = "name"
)

// Memory is the built-in agent-loop tool over the memory store
// (.agents/spec/15-agent-loop.md, "Tools"), which is how an agent remembers
// across runs. Wiring registers it only when a memory store is configured.
//
// Params: op ("search", "add", or "load"), query plus optional limit for
// search, content plus optional importance and tags for add, optional
// token_budget for load, and scope on every op.
type Memory struct {
	store memory.Store
}

// NewMemoryTool builds the tool over a memory store.
func NewMemoryTool(store memory.Store) *Memory {
	return &Memory{store: store}
}

// MemoryToolFactory adapts NewMemoryTool to the registry contract, closing
// over the store because the registry only passes config blocks.
func MemoryToolFactory(store memory.Store) runner.Factory {
	return func(_ map[string]any) (runner.Runner, error) {
		return NewMemoryTool(store), nil
	}
}

// Name implements runner.Runner.
func (*Memory) Name() string {
	return MemoryName
}

// Validate implements runner.Runner: op is required and, once params are
// resolved, must be a known op with its required argument present.
func (*Memory) Validate(task *runner.Task) error {
	if err := requireStringParam(task, paramOp, MemoryName); err != nil {
		return err
	}

	if !task.Resolved {
		return nil
	}

	operation, _ := task.Params[paramOp].(string)

	switch operation {
	case memOpSearch:
		return requireStringParam(task, paramQuery, MemoryName)
	case memOpAdd:
		return requireStringParam(task, paramContent, MemoryName)
	case memOpLoad:
		return nil
	default:
		return taskError(runner.ClassPermanent, MemoryName+".bad_op", "%s", badOpMessage)
	}
}

// Execute implements runner.Runner.
func (m *Memory) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	if err := m.Validate(task); err != nil {
		return runner.Result{}, err
	}

	start := time.Now()

	operation, _ := task.Params[paramOp].(string)
	scope, _ := task.Params[paramScope].(string)

	output, err := m.perform(ctx, operation, scope, task)
	if err != nil {
		return runner.Result{}, err
	}

	return runner.Result{
		Output: output,
		Usage:  runner.Usage{Latency: time.Since(start)},
		Meta:   map[string]string{paramOp: operation},
	}, nil
}

// Describe implements runner.Describer. The scope argument is deliberately
// absent: the agent loop injects it from the action's memory_scope.
func (*Memory) Describe() runner.ToolSpec {
	return runner.ToolSpec{
		Name: MemoryName,
		Description: "Durable memory across workflow runs. " +
			"Use op=search with query to find saved notes, op=load to fetch the most important notes, " +
			"and op=add with content (plus optional importance 0-100 and tags) to save something worth remembering.",
		InputSchema: map[string]any{
			schemaType: schemaObject,
			schemaProperties: map[string]any{
				paramOp: map[string]any{
					schemaType: schemaString,
					"enum":     []string{memOpSearch, memOpAdd, memOpLoad},
				},
				paramQuery:      map[string]any{schemaType: schemaString, schemaDesc: "keywords for op=search"},
				paramContent:    map[string]any{schemaType: schemaString, schemaDesc: "the note to save for op=add"},
				paramImportance: map[string]any{schemaType: schemaInteger, schemaDesc: "0-100 ordering weight for op=add"},
				paramTags:       map[string]any{schemaType: schemaArray, schemaItems: map[string]any{schemaType: schemaString}},
				paramLimit:      map[string]any{schemaType: schemaInteger, schemaDesc: "max results for op=search"},
				paramBudget:     map[string]any{schemaType: schemaInteger, schemaDesc: "token budget for op=load"},
			},
			schemaRequired: []string{paramOp},
		},
	}
}

// perform routes one op to the store.
func (m *Memory) perform(ctx context.Context, operation, scope string, task *runner.Task) (any, error) {
	switch operation {
	case memOpSearch:
		return m.search(ctx, scope, task)
	case memOpAdd:
		return m.add(ctx, scope, task)
	case memOpLoad:
		return m.load(ctx, scope, task)
	default:
		return nil, taskError(runner.ClassPermanent, MemoryName+".bad_op", "%s", badOpMessage)
	}
}

// search runs a keyword (or semantic, when configured) query in the scope.
func (m *Memory) search(ctx context.Context, scope string, task *runner.Task) (any, error) {
	query, _ := task.Params[paramQuery].(string)

	limit := defaultMemorySearchLimit
	if value, isNum := toFloat(task.Params[paramLimit]); isNum && value > 0 {
		limit = int(value)
	}

	found, err := m.store.Search(ctx, memory.Query{Scope: scope, Text: query, Limit: limit})
	if err != nil {
		return nil, storeFailure("search memory", err)
	}

	return map[string]any{
		keyEntries:  renderEntries(found.Entries),
		keyFallback: found.Fallback,
	}, nil
}

// add writes one note back into the scope.
func (m *Memory) add(ctx context.Context, scope string, task *runner.Task) (any, error) {
	content, _ := task.Params[paramContent].(string)

	entry := &memory.Entry{Scope: scope, Content: content}

	if value, isNum := toFloat(task.Params[paramImportance]); isNum {
		entry.Importance = int(value)
	}

	entry.Tags = stringList(task.Params[paramTags])

	id, err := m.store.Add(ctx, entry)
	if err != nil {
		return nil, storeFailure("add memory", err)
	}

	return map[string]any{keyID: id}, nil
}

// load pulls the scope's most important entries under a token budget.
func (m *Memory) load(ctx context.Context, scope string, task *runner.Task) (any, error) {
	budget := defaultMemoryLoadBudget
	if value, isNum := toFloat(task.Params[paramBudget]); isNum && value > 0 {
		budget = int(value)
	}

	entries, err := m.store.Load(ctx, scope, budget)
	if err != nil {
		return nil, storeFailure("load memory", err)
	}

	return map[string]any{keyEntries: renderEntries(entries)}, nil
}

// storeFailure classifies a memory-store error. Permanent because the same
// op against the same store usually fails the same way (a malformed query,
// a closed database); the driver can rephrase rather than retry verbatim.
func storeFailure(operation string, err error) error {
	return &runner.Error{
		Class: runner.ClassPermanent, Code: MemoryName + ".store",
		Message: operation + ": " + err.Error(), Cause: err,
	}
}

// renderEntries maps store entries onto the JSON-friendly shape drivers
// read back out of the tool result.
func renderEntries(entries []memory.Entry) []map[string]any {
	out := make([]map[string]any, 0, len(entries))

	for idx := range entries {
		out = append(out, map[string]any{
			keyID:           entries[idx].ID,
			paramContent:    entries[idx].Content,
			paramImportance: entries[idx].Importance,
			paramTags:       entries[idx].Tags,
		})
	}

	return out
}

// stringList reads an optional []any of strings (the JSON shape tool
// arguments arrive in), skipping non-string elements rather than failing:
// a driver's tags are advisory, not a contract.
func stringList(value any) []string {
	items, isList := value.([]any)
	if !isList {
		return nil
	}

	texts := make([]string, 0, len(items))

	for _, item := range items {
		if text, isText := item.(string); isText {
			texts = append(texts, text)
		}
	}

	return texts
}
