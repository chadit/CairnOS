package builtins

import (
	"context"
	"time"

	"github.com/chadit/CairnOS/internal/vault"
	"github.com/chadit/CairnOS/pkg/runner"
)

// VaultQueryName is the registry name of the vault query built-in tool. It is
// read-only: the chat agent reads the knowledge graph but never writes it
// through this tool (.agents/spec/13-roadmap.md, Phase 9).
const VaultQueryName = "vault_query"

// Vault query tool params. paramTags and paramProject are shared with the
// memory and rules tools.
const (
	paramText        = "text"
	paramLinkedFrom  = "linked_from"
	paramHops        = "hops"
	paramVaultDocs   = "documents"
	defaultVaultDocs = 10
)

// VaultQuery is the built-in agent-loop tool over the vault's typed graph
// query (.agents/spec/15-agent-loop.md, "Tools"; the vault DSL in
// .agents/spec/14-open-decisions.md, S1). It exposes the read side of the
// vault to a chat agent: keyword text match, tag and project filters, and
// bounded link traversal. There is no write op, so a chat turn can read the
// knowledge graph but cannot mutate it through this tool.
type VaultQuery struct {
	store vault.Store
}

// NewVaultQueryTool builds the tool over a vault store.
func NewVaultQueryTool(store vault.Store) *VaultQuery {
	return &VaultQuery{store: store}
}

// VaultQueryToolFactory adapts NewVaultQueryTool to the registry contract,
// closing over the store because the registry only passes config blocks.
func VaultQueryToolFactory(store vault.Store) runner.Factory {
	return func(_ map[string]any) (runner.Runner, error) {
		return NewVaultQueryTool(store), nil
	}
}

// Name implements runner.Runner.
func (*VaultQuery) Name() string {
	return VaultQueryName
}

// Validate implements runner.Runner. Every field is optional and AND-combined,
// so an empty query lists the vault; there is nothing required to validate.
func (*VaultQuery) Validate(_ *runner.Task) error {
	return nil
}

// Execute implements runner.Runner, running the typed graph query and
// rendering matched documents into a JSON-friendly shape the driver reads.
func (v *VaultQuery) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	start := time.Now()

	result, err := v.store.Query(ctx, v.queryFrom(task))
	if err != nil {
		return runner.Result{}, storeFailure("query vault", err)
	}

	return runner.Result{
		Output: map[string]any{
			paramVaultDocs: renderVaultDocs(result.Documents),
			"fallback":     result.Fallback,
		},
		Usage: runner.Usage{Latency: time.Since(start)},
	}, nil
}

// Describe implements runner.Describer.
func (*VaultQuery) Describe() runner.ToolSpec {
	return runner.ToolSpec{
		Name: VaultQueryName,
		Description: "Read the knowledge-graph vault. " +
			"Use text for a keyword search over titles and bodies, tags to narrow to documents carrying every tag, " +
			"project to scope to one project, and linked_from with hops to follow a document's links. " +
			"This tool only reads; it never changes the vault.",
		InputSchema: map[string]any{
			schemaType: schemaObject,
			schemaProperties: map[string]any{
				paramText:       map[string]any{schemaType: schemaString, schemaDesc: "keyword search over title and body"},
				paramTags:       map[string]any{schemaType: schemaArray, schemaItems: map[string]any{schemaType: schemaString}},
				paramProject:    map[string]any{schemaType: schemaString, schemaDesc: "narrow to one project"},
				paramLinkedFrom: map[string]any{schemaType: schemaString, schemaDesc: "document id whose links to follow"},
				paramHops:       map[string]any{schemaType: schemaInteger, schemaDesc: "link traversal depth from linked_from"},
				paramLimit:      map[string]any{schemaType: schemaInteger, schemaDesc: "max documents to return"},
			},
		},
	}
}

// queryFrom maps the tool arguments onto a vault.Query, defaulting the limit
// so an open query does not return an unbounded result into the prompt.
func (*VaultQuery) queryFrom(task *runner.Task) vault.Query {
	query := vault.Query{
		Text:       stringParam(task, paramText),
		Tags:       stringList(task.Params[paramTags]),
		Project:    stringParam(task, paramProject),
		LinkedFrom: stringParam(task, paramLinkedFrom),
		Limit:      defaultVaultDocs,
	}

	if value, isNum := toFloat(task.Params[paramHops]); isNum && value > 0 {
		query.Hops = int(value)
	}

	if value, isNum := toFloat(task.Params[paramLimit]); isNum && value > 0 {
		query.Limit = int(value)
	}

	return query
}

// renderVaultDocs maps documents onto the JSON shape the driver reads back,
// trimming to the fields a chat agent reasons over.
func renderVaultDocs(documents []vault.Document) []map[string]any {
	out := make([]map[string]any, 0, len(documents))

	for idx := range documents {
		out = append(out, map[string]any{
			keyID:     documents[idx].ID,
			"title":   documents[idx].Title,
			"body":    documents[idx].Body,
			paramTags: documents[idx].Tags,
			"links":   documents[idx].Links,
		})
	}

	return out
}

// stringParam reads an optional string param, returning "" when absent or of
// another type, matching the lenient reads the other built-in tools make.
func stringParam(task *runner.Task, key string) string {
	value, _ := task.Params[key].(string)

	return value
}
