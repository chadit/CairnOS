package mcp

import (
	"context"
	"errors"

	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/store"
)

// Canonical auth error codes, matching what the gRPC interceptor answers with:
// a missing scope is auth.forbidden, every token failure collapses to
// auth.unauthenticated so an attacker learns nothing about which check tripped,
// and an expired token gets its own code so a client knows to re-login. Defined
// here rather than imported from internal/api to keep the import graph acyclic.
const (
	codeAuthUnauthenticated = "auth.unauthenticated"
	codeAuthForbidden       = "auth.forbidden"
	codeAuthTokenExpired    = "auth.token_expired" // #nosec G101 -- canonical error code name, not a credential
)

// Authenticator is the slice of internal/auth the MCP server needs;
// *auth.Authenticator satisfies it. A nil Authenticator means auth is disabled:
// every call authorizes and reads the default workspace, matching the rest of
// the API.
type Authenticator interface {
	// Verify checks a bearer token end to end (signature, claims, denylist)
	// and returns its claims for scope enforcement.
	Verify(ctx context.Context, token string) (*auth.Claims, error)
}

// authError is a scope or token failure carrying a canonical auth.* code. The
// tool and resource handlers translate it into the MCP error the SDK sends back
// to the caller, so a forbidden MCP call reads the same as a forbidden gRPC
// call.
type authError struct {
	code    string
	message string
}

// Error implements error, rendering "code: message" so a client that only logs
// the string still sees the canonical code.
func (e *authError) Error() string {
	return e.code + ": " + e.message
}

// authorize runs the same verify-then-scope path the gRPC interceptor runs:
// with auth disabled it passes on the default workspace, otherwise the token
// must verify and its scopes must cover the required one. It returns the
// caller's workspace so the handler's store queries filter by the right tenant,
// the same property the gRPC guard gives unary handlers.
func (s *Service) authorize(ctx context.Context, token, requiredScope string) (string, error) {
	if s.authenticator == nil {
		return store.DefaultWorkspace, nil
	}

	if token == "" {
		return store.DefaultWorkspace, &authError{
			code:    codeAuthUnauthenticated,
			message: "missing bearer token: present Authorization: Bearer <token>",
		}
	}

	claims, err := s.authenticator.Verify(ctx, token)
	if err != nil {
		return store.DefaultWorkspace, verifyError(err)
	}

	if !auth.HasScope(claims.Scopes, requiredScope) {
		return store.DefaultWorkspace, &authError{
			code:    codeAuthForbidden,
			message: "this call requires the " + requiredScope + " scope",
		}
	}

	return claims.Workspace, nil
}

// verifyError maps a token verification failure onto the canonical wire shape,
// the same split the gRPC guard makes: expiry gets its own code so the client
// re-logs in, every other failure collapses to one opaque auth.unauthenticated
// that tells an attacker nothing about which check tripped.
func verifyError(err error) error {
	if errors.Is(err, auth.ErrTokenExpired) {
		return &authError{
			code:    codeAuthTokenExpired,
			message: "token expired: exchange your access key for a fresh one",
		}
	}

	return &authError{code: codeAuthUnauthenticated, message: "invalid token"}
}
