package memory

import (
	"context"

	"github.com/chadit/CairnOS/internal/store"
)

// workspaceFromContext reads the request workspace the interceptor stamped on
// the context, falling back to the default workspace when none is present.
// Memory entries isolate by workspace the same way runs do
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy"): the boundary
// lives on the context, set once at the auth interceptor and read by every
// store query. Reusing store's helper keeps both stores reading one key, so a
// workspace set for a run query also scopes a memory query on the same call.
func workspaceFromContext(ctx context.Context) string {
	return store.WorkspaceFromContext(ctx)
}
