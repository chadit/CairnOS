package api

import (
	"context"

	"github.com/chadit/CairnOS/internal/auth"
)

// scopesCtxKey is the unexported type for the granted-scopes context value, so
// no other package can collide with this key.
type scopesCtxKey struct{}

// contextWithScopes returns a copy of ctx carrying the verified token's granted
// scopes, so a handler can make a second, finer-grained scope check the method
// table cannot express. The chat confirm path needs this: SendMessage takes
// chat:session, but only a confirmed workflow trigger additionally needs
// write:workflow, and that check happens inside ConfirmAction
// (.agents/spec/14-open-decisions.md, D9). With auth disabled, no scopes ride
// the context and ScopesFromContext reports an open set.
func contextWithScopes(ctx context.Context, scopes []string) context.Context {
	return context.WithValue(ctx, scopesCtxKey{}, scopes)
}

// scopesFromContext reads the granted scopes off the context. The second result
// is false when no scopes were stamped, which is the auth-disabled case: every
// in-handler scope check then passes, matching the interceptor's behavior of
// not running at all when auth is off.
func scopesFromContext(ctx context.Context) ([]string, bool) {
	scopes, ok := ctx.Value(scopesCtxKey{}).([]string)

	return scopes, ok
}

// hasScope reports whether the handler's context carries a scope. When no
// scopes were stamped (auth disabled) it returns true, so a handler-level check
// never blocks a node running without auth, the same posture the interceptor
// takes.
func hasScope(ctx context.Context, required string) bool {
	scopes, present := scopesFromContext(ctx)
	if !present {
		return true
	}

	return auth.HasScope(scopes, required)
}
