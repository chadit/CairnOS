package api

import (
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/engine"
)

// API-layer canonical codes (api.* prefix; see the package doc). Engine and
// workflow codes pass through untouched.
const (
	codeWorkflowNotFound   = "api.workflow_not_found"
	codeRunNotFound        = "api.run_not_found"
	codeMemoryNotFound     = "api.memory_not_found"
	codeDocumentNotFound   = "api.document_not_found"
	codeUnknownLanguage    = "api.unknown_language"
	codeBreakerNotFound    = "api.breaker_not_found"
	codeSuggestionNotFound = "api.suggestion_not_found"
	codeRunFinished        = "api.run_finished"
	codeRunNotManaged      = "api.run_not_managed"
	codeReplayHashMismatch = "api.replay_hash_mismatch"
	// codeWorkflowNameMismatch reports a save or inline run whose definition
	// name differs from the {workflow_name} path param. Without the check a
	// save could strand a file the resolver maps under a different name than
	// the URL the client used (.agents/spec/parity/builder-ops.md).
	codeWorkflowNameMismatch = "api.workflow_name_mismatch"
	codeInvalidArgument      = "api.invalid_argument"
	codeUnimplemented        = "api.unimplemented"
	codeInternal             = "api.internal"
)

// Canonical agent.* codes from the 04 mapping table. The agent loop ships in
// Phase 1c; the mapping is wired now so its errors surface correctly the day
// they exist.
const (
	codeAgentBudgetExhausted = "agent.budget_exhausted"
	codeAgentMaxIterations   = "agent.max_iterations"
	codeAgentBadToolCall     = "agent.bad_tool_call"
	codeAgentDriverIncapable = "agent.driver_incapable"
)

// Canonical auth.* codes (.agents/spec/08-auth-and-access.md, "Scopes"):
// a missing or bad token is auth.unauthenticated, a token past exp is
// auth.token_expired (both UNAUTHENTICATED), a valid token without the
// method's scope is auth.forbidden (PERMISSION_DENIED via the auth.*
// prefix fallback), and a login against a server running with auth
// disabled is auth.disabled (FAILED_PRECONDITION).
const (
	codeAuthUnauthenticated = "auth.unauthenticated"
	codeAuthTokenExpired    = "auth.token_expired" // #nosec G101 -- canonical error code name, not a credential
	codeAuthForbidden       = "auth.forbidden"
	codeAuthDisabled        = "auth.disabled"

	// codeAuthTooManyLogins maps to RESOURCE_EXHAUSTED: an unauthenticated
	// Login flood that the concurrency cap or the per-peer rate limit refused
	// before it could pin memory in argon2id work (spec M1).
	codeAuthTooManyLogins = "auth.too_many_logins"
)

// Canonical code prefixes used by the family fallback in grpcCodeFor.
const (
	prefixWorkflow = "workflow."
	prefixConfig   = "config."
	prefixAuth     = "auth."
	prefixAPI      = "api."
)

// grpcCodeFor maps a canonical dotted code to its gRPC status code, per the
// table in .agents/spec/04-execution-engine.md ("Mapping to gRPC status
// codes"). Exact codes first, then family prefixes, then INTERNAL.
func grpcCodeFor(code string) codes.Code {
	if mapped, exact := exactGRPCCode(code); exact {
		return mapped
	}

	switch {
	case strings.HasPrefix(code, prefixWorkflow), strings.HasPrefix(code, prefixConfig):
		return codes.InvalidArgument
	case strings.HasPrefix(code, prefixAuth):
		return codes.PermissionDenied
	case strings.HasPrefix(code, prefixAPI):
		return apiGRPCCode(code)
	default:
		return codes.Internal
	}
}

// exactGRPCCode covers the table rows keyed by a specific code.
func exactGRPCCode(code string) (codes.Code, bool) {
	switch code {
	case engine.CodeRunnerRateLimited, codeAgentBudgetExhausted, codeAgentMaxIterations, codeAuthTooManyLogins:
		return codes.ResourceExhausted, true
	case engine.CodeCircuitOpen:
		return codes.Unavailable, true
	case engine.CodeActionTimeout, engine.CodeRunTimeout:
		return codes.DeadlineExceeded, true
	case engine.CodeCanceled:
		return codes.Canceled, true
	case engine.CodePreconditionFailed, codeAgentBadToolCall, codeAgentDriverIncapable:
		return codes.FailedPrecondition, true
	case engine.CodeRunnerPermanent:
		return codes.InvalidArgument, true
	case codeAuthUnauthenticated, codeAuthTokenExpired:
		return codes.Unauthenticated, true
	case codeAuthDisabled:
		return codes.FailedPrecondition, true
	default:
		return codes.Internal, false
	}
}

// apiGRPCCode maps the API-layer codes the transport itself produces.
func apiGRPCCode(code string) codes.Code {
	switch code {
	case codeWorkflowNotFound, codeRunNotFound, codeMemoryNotFound, codeBreakerNotFound, codeSuggestionNotFound:
		return codes.NotFound
	case codeRunFinished, codeRunNotManaged:
		return codes.FailedPrecondition
	case codeInvalidArgument, codeWorkflowNameMismatch:
		return codes.InvalidArgument
	case codeUnimplemented:
		return codes.Unimplemented
	default:
		return codes.Internal
	}
}

// protoError renders a canonical error for the wire: status details on gRPC,
// the error envelope over HTTP, and RunEvent.error in streams. Details that
// cannot become a Struct (non-JSON values) are dropped rather than failing
// the whole response; the code and message still travel.
func protoError(cerr *cairnerr.Error) *cairnosv1.Error {
	if cerr == nil {
		return nil
	}

	out := &cairnosv1.Error{Code: cerr.Code, Message: cerr.Message}

	if len(cerr.Details) > 0 {
		if details, err := structpb.NewStruct(cerr.Details); err == nil {
			out.Details = details
		}
	}

	return out
}

// errorStatus builds the gRPC error for one canonical failure, carrying the
// full structured error in the status details so gRPC and HTTP clients see
// the same code (.agents/spec/07-api-protocols.md, "HTTP response (error)").
//
//nolint:wrapcheck // status errors are the gRPC transport contract; wrapping them adds nothing for handlers that all funnel through here
func errorStatus(grpcCode codes.Code, cerr *cairnerr.Error) error {
	base := status.New(grpcCode, cerr.Message)

	detailed, err := base.WithDetails(protoError(cerr))
	if err != nil {
		// The detail failed to marshal; the bare status still carries the
		// right gRPC code and message.
		return base.Err()
	}

	return detailed.Err()
}

// canonicalStatus classifies any error into the canonical gRPC shape:
// cairnerr values map through the 04 table, everything else is INTERNAL.
func canonicalStatus(err error) error {
	if list, isList := errors.AsType[cairnerr.Errors](err); isList && len(list) > 0 {
		return errorStatus(grpcCodeFor(list[0].Code), aggregateError(list))
	}

	if cerr, isCanonical := errors.AsType[*cairnerr.Error](err); isCanonical {
		return errorStatus(grpcCodeFor(cerr.Code), cerr)
	}

	return errorStatus(codes.Internal, cairnerr.New(codeInternal, err.Error()))
}

// aggregateError folds a validation error list into one canonical error: the
// first code leads (it decides the gRPC code), and every individual problem
// rides in details.errors so clients can fix a file in one pass.
func aggregateError(list cairnerr.Errors) *cairnerr.Error {
	items := make([]any, 0, len(list))
	for _, item := range list {
		items = append(items, map[string]any{"code": item.Code, "message": item.Message})
	}

	return &cairnerr.Error{
		Code:    list[0].Code,
		Message: list.Error(),
		Details: map[string]any{"errors": items},
	}
}
