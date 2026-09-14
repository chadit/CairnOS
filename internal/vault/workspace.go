package vault

import (
	"context"

	"github.com/chadit/CairnOS/internal/store"
)

// projectTagPrefix is the tag convention a project filter rides on. The vault
// stores no separate project column; a project is the tag
// "project:finance-app", so Query.Project narrows by that synthesized tag
// (.agents/spec/05-memory-and-knowledge.md, "all documents tagged X for
// project Y"). One table, one filter mechanism.
const projectTagPrefix = "project:"

// projectTag renders a project name as its tag. An empty project yields the
// empty string so callers can skip the filter.
func projectTag(project string) string {
	if project == "" {
		return ""
	}

	return projectTagPrefix + project
}

// workspaceFromContext reads the request workspace the interceptor stamped on
// the context, falling back to the default when none is present. Vault
// documents isolate the way memory entries and runs do
// (.agents/spec/12-ui-and-governance.md): the boundary lives on the context.
// Reusing store's helper keeps every store reading one key.
func workspaceFromContext(ctx context.Context) string {
	return store.WorkspaceFromContext(ctx)
}
