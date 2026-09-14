package rules

import (
	"context"

	"github.com/chadit/CairnOS/internal/store"
)

// workspaceFromContext reads the request workspace the interceptor stamped on
// the context, falling back to the default when none is present. Rules isolate
// by workspace the same way memory, vault, and runs do
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy"): the boundary lives on
// the context, set once at the auth interceptor, read by every store query.
// Reusing store's helper keeps every store on one key.
func workspaceFromContext(ctx context.Context) string {
	return store.WorkspaceFromContext(ctx)
}
