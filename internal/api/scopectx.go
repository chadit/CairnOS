package api

import (
	"context"

	"github.com/chadit/CairnOS/internal/auth"
)

// scopesCtxKey is the unexported type for the granted-scopes context value, so
// no other package can collide with this key.
type scopesCtxKey struct{}

// contextWithScopes returns a copy of ctx carrying the verified token's granted
// scopes, so a handler can make a second, finer check the method table cannot
// express. SendMessage takes chat:session, but a confirmed workflow trigger also
// needs write:workflow, and that check happens inside ConfirmAction. With
// auth off no scopes ride the context and the reader reports an open set.
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

// subjectCtxKey is the unexported type for the caller-subject context value.
type subjectCtxKey struct{}

// contextWithSubject returns a copy of ctx carrying the verified token's
// subject: the key id for an access-key token, the IdP subject for an OAuth
// login. A handler telling one caller from another within a tenant needs it,
// since the A2A path matches a proposal against the peer that opened it.
// With auth off nothing is stamped.
func contextWithSubject(ctx context.Context, subject string) context.Context {
	return context.WithValue(ctx, subjectCtxKey{}, subject)
}

// subjectFromContext reads the caller's subject off the context. The second
// result is false when no token rode the call, which is the auth-disabled and
// public-method case.
func subjectFromContext(ctx context.Context) (string, bool) {
	subject, ok := ctx.Value(subjectCtxKey{}).(string)

	return subject, ok
}
