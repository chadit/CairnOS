package store

import "context"

// DefaultWorkspace is the tenancy boundary every row falls into when no
// workspace rides the request context. Access-key logins and auth-disabled
// local use both land here, so single-tenant setups behave exactly as they did
// before multi-tenancy: one workspace named "default".
const DefaultWorkspace = "default"

// workspaceCtxKey is the unexported type for the workspace context value, so
// no other package can collide with this key.
type workspaceCtxKey struct{}

// ContextWithWorkspace returns a copy of ctx carrying the workspace id every
// store query filters by and the engine stamps onto new runs. An empty id
// stores DefaultWorkspace, so a caller never accidentally narrows queries to
// the empty-string workspace. The interceptor sets this from the verified
// token's workspace claim; the engine and the store read it back.
func ContextWithWorkspace(ctx context.Context, workspace string) context.Context {
	if workspace == "" {
		workspace = DefaultWorkspace
	}

	return context.WithValue(ctx, workspaceCtxKey{}, workspace)
}

// WorkspaceFromContext reads the request workspace, returning DefaultWorkspace
// when none was set. That fallback is what keeps every existing caller and
// the auth-disabled path on the default workspace without a code change: a
// context with no workspace value reads as "default", the pre-tenancy
// behavior.
func WorkspaceFromContext(ctx context.Context) string {
	workspace, ok := ctx.Value(workspaceCtxKey{}).(string)
	if !ok || workspace == "" {
		return DefaultWorkspace
	}

	return workspace
}
