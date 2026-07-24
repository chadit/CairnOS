package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/store"
)

// Authenticator is the slice of internal/auth the API needs;
// *auth.Authenticator satisfies it. A nil Authenticator in Config means
// auth is disabled: no interceptor is installed and every call passes.
type Authenticator interface {
	// Login exchanges a plaintext access key for a token and its expiry.
	Login(ctx context.Context, key string) (string, time.Time, error)

	// Verify checks a token end to end (signature, claims, denylist) and
	// returns its claims for scope enforcement.
	Verify(ctx context.Context, token string) (*auth.Claims, error)

	// RevokeToken denylists one token id.
	RevokeToken(ctx context.Context, jti string) error

	// RevokeKey denylists every live token minted from one access key.
	RevokeKey(ctx context.Context, keyID string) error
}

// Metadata keys a bearer token may arrive under: native gRPC clients send
// "authorization"; grpc-gateway forwards the HTTP Authorization header as
// a permanent header under its own prefix.
const (
	authorizationKey        = "authorization"
	gatewayAuthorizationKey = "grpcgateway-authorization"
)

// bearerScheme is the accepted Authorization scheme; the comparison is
// case-insensitive per RFC 9110.
const bearerScheme = "bearer "

// scopeForMethod maps one full gRPC method name onto its required scope,
// per the tables in .agents/spec/07-api-protocols.md. The second result
// reports a public method needing no token: Health (liveness must work
// from probes) and Login (the way in cannot demand a token).
//
// SuggestionService is absent from the 07 tables (it postdates them), so
// the mapping follows the same logic the tables apply elsewhere: listing
// suggestions reads run-derived history (read:run, like GetRun/ListRuns),
// while approve/reject change operator state and sit with the other
// operator mutations under admin.
//
// Methods not named here (server reflection, future services) fail closed
// to admin rather than passing unchecked; extending the API means
// extending this table.
func scopeForMethod(fullMethod string) (string, bool) {
	switch fullMethod {
	case "/cairnos.v1.AdminService/Health", "/cairnos.v1.LoginService/Login":
		return "", true
	case "/cairnos.v1.WorkflowService/RunWorkflow", "/cairnos.v1.WorkflowService/CancelRun",
		"/cairnos.v1.WorkflowService/ReplayRun", "/cairnos.v1.WorkflowService/SaveWorkflow":
		return auth.ScopeWriteWorkflow, false
	case "/cairnos.v1.WorkflowService/ValidateWorkflow", "/cairnos.v1.AdminService/ListRunners":
		return auth.ScopeReadWorkflow, false
	case "/cairnos.v1.WorkflowService/GetRun", "/cairnos.v1.WorkflowService/ListRuns",
		"/cairnos.v1.SuggestionService/ListSuggestions":
		return auth.ScopeReadRun, false
	case "/cairnos.v1.MemoryService/GetMemory", "/cairnos.v1.MemoryService/SearchMemory":
		return auth.ScopeReadMemory, false
	case "/cairnos.v1.MemoryService/AddMemory", "/cairnos.v1.MemoryService/ReplaceMemory",
		"/cairnos.v1.MemoryService/DeleteMemory":
		return auth.ScopeWriteMemory, false
	case "/cairnos.v1.VaultService/GetDocument", "/cairnos.v1.VaultService/QueryVault",
		"/cairnos.v1.LanguageRulesService/GetRules", "/cairnos.v1.LanguageRulesService/ListLanguages":
		// Reading documents and rules takes read:vault. The rules service is a
		// typed view over the vault, so its reads sit under the vault read
		// scope rather than a separate rules scope
		// (.agents/spec/14-open-decisions.md, S1 note).
		return auth.ScopeReadVault, false
	case "/cairnos.v1.VaultService/PutDocument", "/cairnos.v1.VaultService/DeleteDocument",
		"/cairnos.v1.LanguageRulesService/PutRule":
		// Writing documents and rules takes write:vault.
		return auth.ScopeWriteVault, false
	case "/cairnos.v1.ChatService/CreateSession", "/cairnos.v1.ChatService/SendMessage",
		"/cairnos.v1.ChatService/ConfirmAction", "/cairnos.v1.ChatService/GetSession",
		"/cairnos.v1.ChatService/ListSessions", "/cairnos.v1.ChatService/ListMessages",
		"/cairnos.v1.ChatService/SearchMessages":
		// Every chat method takes chat:session (.agents/spec/13-roadmap.md,
		// Phase 9). Approving a workflow trigger additionally needs
		// write:workflow, which ConfirmAction checks in-handler from the
		// stamped scopes, since the method table grants one scope per method
		// (.agents/spec/14-open-decisions.md, D9).
		return auth.ScopeChatSession, false
	case "/cairnos.v1.AdminService/ResetBreaker",
		"/cairnos.v1.SuggestionService/ApproveSuggestion", "/cairnos.v1.SuggestionService/RejectSuggestion",
		"/cairnos.v1.LoginService/Revoke",
		"/cairnos.v1.GovernanceService/GetWorkflowBudgetState", "/cairnos.v1.GovernanceService/PauseWorkflow",
		"/cairnos.v1.GovernanceService/ResumeWorkflow", "/cairnos.v1.GovernanceService/TerminateRun",
		"/cairnos.v1.GovernanceService/ListAudit":
		// Every governance method takes the admin scope: pause, resume, and
		// terminate are admin control actions (.agents/spec/12-ui-and-governance.md,
		// "Control actions"), and the budget-state and audit reads are operator
		// concerns that sit alongside them rather than with run history.
		return auth.ScopeAdmin, false
	case "/cairnos.a2a.v1.A2AService/CreateTask", "/cairnos.a2a.v1.A2AService/GetTask",
		"/cairnos.a2a.v1.A2AService/ListTasks", "/cairnos.a2a.v1.A2AService/CancelTask",
		"/cairnos.a2a.v1.A2AService/TaskUpdates":
		// Every A2A method takes the single a2a:task scope
		// (.agents/spec/09-a2a-and-hive.md, "Core methods"). The method names
		// are literals rather than a2a package constants because importing
		// internal/a2a here would cycle: a2a depends on this package.
		return auth.ScopeA2ATask, false
	default:
		return auth.ScopeAdmin, false
	}
}

// defaultStreamRecheck is the fallback re-verify cadence for open streams
// when Config.StreamRecheck is unset (spec M4).
const defaultStreamRecheck = 15 * time.Second

// authGuard enforces the Phase 4a token check on every RPC
// (.agents/spec/08-auth-and-access.md, "Scopes"): validate the bearer
// token, consult the denylist, and require the method's scope before the
// handler runs.
type authGuard struct {
	authenticator Authenticator

	// streamRecheck bounds how often an open stream re-verifies its token
	// (spec M4): a long-lived stream must not keep delivering after the token
	// expires or is revoked. now injects the clock so tests drive the recheck
	// without sleeping.
	streamRecheck time.Duration
	now           func() time.Time
}

// newAuthGuard wires the interceptor pair over one authenticator. A
// non-positive recheck falls back to the default; nil now uses time.Now.
func newAuthGuard(authenticator Authenticator, streamRecheck time.Duration, now func() time.Time) *authGuard {
	if streamRecheck <= 0 {
		streamRecheck = defaultStreamRecheck
	}

	if now == nil {
		now = time.Now
	}

	return &authGuard{authenticator: authenticator, streamRecheck: streamRecheck, now: now}
}

// unary is the grpc.UnaryServerInterceptor face of the guard. The verified
// token's workspace rides onto the context so the handler's store queries
// filter by the caller's tenant (.agents/spec/12-ui-and-governance.md,
// "Multi-tenancy").
func (g *authGuard) unary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	workspace, scopes, err := g.authorize(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}

	authed := contextWithScopes(store.ContextWithWorkspace(ctx, workspace), scopes)

	return handler(authed, req)
}

// stream is the grpc.StreamServerInterceptor face of the guard: the scope
// check runs once before the stream opens, then a wrapper re-verifies the
// token's expiry and denylist state on each send, throttled to streamRecheck,
// so revoking or expiring a token ends a long-lived stream (spec M4). Public
// methods skip the wrapper since they carried no token to re-check.
func (g *authGuard) stream(srv any, serverStream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	workspace, scopes, err := g.authorize(serverStream.Context(), info.FullMethod)
	if err != nil {
		return err
	}

	if _, public := scopeForMethod(info.FullMethod); public {
		return handler(srv, serverStream)
	}

	token, err := bearerToken(serverStream.Context())
	if err != nil {
		return err
	}

	// The wrapper's Context stamps the caller's workspace and granted scopes, so
	// the handler's store queries filter by the right tenant and a streaming
	// handler can make a finer-grained scope check (the chat confirm path).
	wrapped := &recheckStream{
		ServerStream:  serverStream,
		workspace:     workspace,
		scopes:        scopes,
		authenticator: g.authenticator,
		token:         token,
		interval:      g.streamRecheck,
		now:           g.now,
		lastCheck:     g.now(),
	}

	return handler(srv, wrapped)
}

// authorize runs the verify path for one call: public methods pass, then
// the bearer token must verify (algorithm, signature, iss/aud, exp, jti,
// denylist), then the token's scopes must cover the method. It returns the
// context the handler should run under, carrying the token's workspace so the
// store filters by the caller's tenant (.agents/spec/12-ui-and-governance.md,
// "Multi-tenancy"). A public method passes the context through unchanged,
// which leaves it on the default workspace.
func (g *authGuard) authorize(ctx context.Context, fullMethod string) (string, []string, error) {
	requiredScope, public := scopeForMethod(fullMethod)
	if public {
		return store.DefaultWorkspace, nil, nil
	}

	token, err := bearerToken(ctx)
	if err != nil {
		return store.DefaultWorkspace, nil, err
	}

	claims, err := g.authenticator.Verify(ctx, token)
	if err != nil {
		return store.DefaultWorkspace, nil, verifyStatus(err)
	}

	if !auth.HasScope(claims.Scopes, requiredScope) {
		return store.DefaultWorkspace, nil, errorStatus(codes.PermissionDenied,
			cairnerr.Newf(codeAuthForbidden, "method %s requires the %q scope", fullMethod, requiredScope))
	}

	return claims.Workspace, claims.Scopes, nil
}

// bearerToken pulls the token out of the call metadata, accepting both
// the native gRPC key and the gateway-forwarded one.
func bearerToken(ctx context.Context) (string, error) {
	meta, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", errorStatus(codes.Unauthenticated,
			cairnerr.New(codeAuthUnauthenticated, "missing Authorization header"))
	}

	for _, key := range []string{authorizationKey, gatewayAuthorizationKey} {
		for _, value := range meta.Get(key) {
			if len(value) > len(bearerScheme) && strings.EqualFold(value[:len(bearerScheme)], bearerScheme) {
				return value[len(bearerScheme):], nil
			}
		}
	}

	return "", errorStatus(codes.Unauthenticated,
		cairnerr.New(codeAuthUnauthenticated, "missing or malformed Authorization header: expected Bearer <token>"))
}

// recheckStream wraps a server stream so each send re-verifies the bearer
// token, throttled to interval (spec M4). gRPC serializes SendMsg on one
// stream, so the throttle state needs no lock. Once a recheck fails the
// stream ends with the auth error, so a token revoked or expired mid-stream
// stops further delivery.
type recheckStream struct {
	grpc.ServerStream

	// workspace is the caller's tenant, lifted from the verified token. Context
	// derives the handler's context from the embedded stream's, so the store
	// queries a streaming handler runs filter by the right tenant. Storing the
	// string rather than the whole context keeps the context request-derived
	// instead of struct-held (.agents/spec/12-ui-and-governance.md,
	// "Multi-tenancy").
	workspace string

	// scopes are the verified token's granted scopes, stamped onto the handler
	// context so a streaming handler can make a finer-grained check the method
	// table cannot express (the chat confirm path's write:workflow gate).
	scopes []string

	authenticator Authenticator
	token         string
	interval      time.Duration
	now           func() time.Time
	lastCheck     time.Time
}

// Context returns the embedded stream's context with the caller's workspace and
// granted scopes stamped on, so a streaming handler reads the tenant the same
// way a unary handler does and can re-check a scope at handler time.
func (r *recheckStream) Context() context.Context {
	return contextWithScopes(store.ContextWithWorkspace(r.ServerStream.Context(), r.workspace), r.scopes)
}

// SendMsg re-verifies the token when interval has elapsed since the last
// check, then forwards the message. A failed recheck returns the auth status
// and never sends, ending the stream.
func (r *recheckStream) SendMsg(msg any) error {
	now := r.now()
	if now.Sub(r.lastCheck) >= r.interval {
		r.lastCheck = now

		if _, err := r.authenticator.Verify(r.Context(), r.token); err != nil {
			return verifyStatus(err)
		}
	}

	if err := r.ServerStream.SendMsg(msg); err != nil {
		return fmt.Errorf("stream send: %w", err)
	}

	return nil
}

// verifyStatus maps an auth verification failure onto the canonical wire
// shape. Expiry gets its own code so clients know to re-login; every
// other failure (bad signature, wrong algorithm, revoked, malformed)
// collapses into auth.unauthenticated, deliberately telling an attacker
// nothing about which check tripped.
func verifyStatus(err error) error {
	if errors.Is(err, auth.ErrTokenExpired) {
		return errorStatus(codes.Unauthenticated,
			cairnerr.New(codeAuthTokenExpired, "token expired: exchange your access key at /v1/login for a fresh one"))
	}

	return errorStatus(codes.Unauthenticated,
		cairnerr.New(codeAuthUnauthenticated, "invalid token"))
}
