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

// ScopeForMethod maps one full gRPC method name onto its required scope, per
// the tables in .agents/spec/07-api-protocols.md. The second result marks a
// public method: Health and Login carry no token.
//
// Exported because this table is the contract an operator reads when minting a
// key: `cairnos keys add --scopes` has to name the scope a method will demand.
func ScopeForMethod(fullMethod string) (string, bool) {
	switch fullMethod {
	case "/cairnos.v1.AdminService/Health", "/cairnos.v1.LoginService/Login":
		return "", true
	case "/cairnos.v1.WorkflowService/RunWorkflow", "/cairnos.v1.WorkflowService/CancelRun",
		"/cairnos.v1.WorkflowService/ReplayRun", "/cairnos.v1.WorkflowService/SaveWorkflow",
		"/cairnos.v1.WorkflowService/ApproveRun":
		// ApproveRun sits with the other run mutations: deciding a gate
		// changes what the run executes, per the D9 pattern of enforcing
		// write:workflow at decision time
		// (.agents/spec/parity/workflow-language.md, "Human approval").
		return auth.ScopeWriteWorkflow, false
	case "/cairnos.v1.WorkflowService/ValidateWorkflow", "/cairnos.v1.AdminService/ListRunners":
		return auth.ScopeReadWorkflow, false
	case "/cairnos.v1.WorkflowService/GetRun", "/cairnos.v1.WorkflowService/ListRuns",
		"/cairnos.v1.SuggestionService/ListSuggestions":
		// SuggestionService postdates the 07 tables. Listing reads run-derived
		// history, so it sits with GetRun; approve and reject change operator
		// state and sit under admin below.
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
	case "/cairnos.v1.AdminService/ResetBreaker", "/cairnos.v1.AdminService/RequestRestart",
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
	case "/cairnos.v1.MessagingService/SendMessage", "/cairnos.v1.MessagingService/ListIdentities":
		// Both messaging methods take the single message:send scope (FR30).
		// ListIdentities sits with the send because its answer is the argument
		// list for a send. The method names are literals rather than package
		// constants, matching the A2A entries below, since internal/messaging
		// depends on this package.
		return auth.ScopeMessageSend, false
	case "/cairnos.a2a.v1.A2AService/CreateTask", "/cairnos.a2a.v1.A2AService/GetTask",
		"/cairnos.a2a.v1.A2AService/ListTasks", "/cairnos.a2a.v1.A2AService/CancelTask",
		"/cairnos.a2a.v1.A2AService/TaskUpdates", "/cairnos.a2a.v1.A2AService/SendMessage":
		// Every A2A method takes the single a2a:task scope
		// (.agents/spec/09-a2a-and-hive.md, "Core methods"). The method names
		// are literals rather than a2a package constants because importing
		// internal/a2a here would cycle: a2a depends on this package.
		return auth.ScopeA2ATask, false
	default:
		// Server reflection and any future service fail closed to admin rather
		// than passing unchecked; extending the API means extending this table.
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

	// now injects the clock so tests drive the stream recheck without sleeping.
	now func() time.Time

	// streamRecheck bounds how often an open stream re-verifies its token
	// (spec M4): a long-lived stream must not keep delivering after the token
	// expires or is revoked.
	streamRecheck time.Duration
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
	caller, err := g.authorize(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}

	return handler(caller.stamp(ctx), req)
}

// stream is the grpc.StreamServerInterceptor face of the guard: the scope
// check runs once before the stream opens, then a wrapper re-verifies the
// token's expiry and denylist state on each send, throttled to streamRecheck,
// so revoking or expiring a token ends a long-lived stream (spec M4). Public
// methods skip the wrapper since they carried no token to re-check.
func (g *authGuard) stream(srv any, serverStream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	caller, err := g.authorize(serverStream.Context(), info.FullMethod)
	if err != nil {
		return err
	}

	if _, public := ScopeForMethod(info.FullMethod); public {
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
		caller:        caller,
		authenticator: g.authenticator,
		token:         token,
		interval:      g.streamRecheck,
		now:           g.now,
		lastCheck:     g.now(),
	}

	return handler(srv, wrapped)
}

// principal is what one verified token grants the handler: the tenant its store
// queries filter by, the subject that names the caller inside that tenant, and
// the scopes a handler can re-check. It is one value rather than three results
// so authorize does not grow another return every time a handler needs a
// further claim.
type principal struct {
	Workspace string
	Subject   string
	Scopes    []string
}

// stamp returns the context the handler runs under. The workspace makes store
// queries filter by the caller's tenant
// (.agents/spec/12-ui-and-governance.md, "Multi-tenancy"), the scopes let a
// handler make a finer check the method table cannot express, and the subject
// tells one caller from another within the tenant.
func (p *principal) stamp(ctx context.Context) context.Context {
	authed := contextWithScopes(store.ContextWithWorkspace(ctx, p.Workspace), p.Scopes)

	if p.Subject == "" {
		return authed
	}

	return contextWithSubject(authed, p.Subject)
}

// authorize runs the verify path for one call: public methods pass, then
// the bearer token must verify (algorithm, signature, iss/aud, exp, jti,
// denylist), then the token's scopes must cover the method. A public method
// carried no token, so it comes back on the default workspace with no subject
// and no scopes, and stamping it leaves the context unchanged.
func (g *authGuard) authorize(ctx context.Context, fullMethod string) (principal, error) {
	anonymous := principal{Workspace: store.DefaultWorkspace}

	requiredScope, public := ScopeForMethod(fullMethod)
	if public {
		return anonymous, nil
	}

	token, err := bearerToken(ctx)
	if err != nil {
		return anonymous, err
	}

	claims, err := g.authenticator.Verify(ctx, token)
	if err != nil {
		return anonymous, verifyStatus(err)
	}

	if !auth.HasScope(claims.Scopes, requiredScope) {
		return anonymous, errorStatus(codes.PermissionDenied,
			cairnerr.Newf(codeAuthForbidden, "method %s requires the %q scope", fullMethod, requiredScope))
	}

	return principal{Workspace: claims.Workspace, Subject: claims.Subject, Scopes: claims.Scopes}, nil
}

// actorFromContext derives who performed a mutating call: the authenticated
// subject when a verifiable bearer token rode the call, else "local". A
// verify failure here never blocks the action (the interceptor already
// authorized it); it only means the record names "local".
func actorFromContext(ctx context.Context, authenticator Authenticator) string {
	if authenticator == nil {
		return actorLocal
	}

	token, err := bearerToken(ctx)
	if err != nil {
		return actorLocal
	}

	claims, err := authenticator.Verify(ctx, token)
	if err != nil || claims.Subject == "" {
		return actorLocal
	}

	return claims.Subject
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

	lastCheck     time.Time
	authenticator Authenticator
	now           func() time.Time
	token         string

	// caller is what the verified token granted. Context stamps it onto the
	// embedded stream's context per call, so a streaming handler reads the same
	// tenant, subject, and scopes a unary handler does. Storing the claims rather
	// than a whole context keeps the handler's context request-derived instead of
	// struct-held.
	caller principal

	interval time.Duration
}

// Context returns the embedded stream's context with the caller's claims
// stamped on, so a streaming handler reads the tenant the same way a unary
// handler does and can re-check a scope at handler time.
func (r *recheckStream) Context() context.Context {
	return r.caller.stamp(r.ServerStream.Context())
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
