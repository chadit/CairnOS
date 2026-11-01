package api

import (
	"context"

	"google.golang.org/grpc/codes"

	"github.com/chadit/CairnOS/internal/cairnerr"
)

// The exports below are the surface internal/a2a and internal/messaging build
// on. Both are protocol adapters over behavior this package fronts (FR26), so
// they reuse this one error-mapping path rather than duplicating the 04 status
// table, and the canonical code strings stay in status.go where the gRPC
// mapping reads them. Keeping the export list small and in one file documents
// the seam. The names carry a Wire suffix to stay clear of the package-private
// helpers they delegate to.

// WireStatus classifies any error into the canonical gRPC shape, mapping
// cairnerr values through the 04 table and everything else to INTERNAL. The
// A2A adapter funnels its store and manager errors through here so an A2A
// client sees the same codes a native API client does.
func WireStatus(err error) error {
	return canonicalStatus(err)
}

// WireRunLookup classifies a store GetRun failure: a missing run becomes
// NotFound with the api.run_not_found code, anything else maps through the
// canonical table.
func WireRunLookup(runID string, err error) error {
	return runLookupStatus(runID, err)
}

// WireRunFinished builds the FailedPrecondition error returned when a cancel
// targets a run that already reached a terminal state.
func WireRunFinished(runID, status string) error {
	return errorStatus(codes.FailedPrecondition,
		cairnerr.Newf(codeRunFinished, "run %s already finished as %s", runID, status))
}

// WireRunNotManaged builds the FailedPrecondition error returned when a cancel
// targets a run this server did not start (finished, unknown, or started on
// another node).
func WireRunNotManaged(runID string) error {
	return errorStatus(codes.FailedPrecondition,
		cairnerr.Newf(codeRunNotManaged, "run %s was not started by this server; cancel it where it was started", runID))
}

// WireInvalidArgument builds the InvalidArgument error for a request the
// messaging service rejects before any transport call: an unparseable target
// or an empty body (FR27). Callers get the same api.invalid_argument code a
// native handler produces for a malformed field.
func WireInvalidArgument(format string, args ...any) error {
	return errorStatus(codes.InvalidArgument, cairnerr.Newf(codeInvalidArgument, format, args...))
}

// WireIdentityNotFound builds the NotFound error for a send addressed to an
// identity the messaging registry does not hold.
func WireIdentityNotFound(name string) error {
	return errorStatus(codes.NotFound,
		cairnerr.Newf(codeIdentityNotFound, "no messaging identity named %q is configured", name))
}

// WireChatDisabled builds the FailedPrecondition error for an A2A conversation
// request on a node whose chat surface is off. A peer gets a precondition
// rather than an empty reply, so a misconfigured node is distinguishable from
// an agent with nothing to say.
func WireChatDisabled() error {
	return errorStatus(codes.FailedPrecondition,
		cairnerr.New(codeChatDisabled, "chat is not enabled on this node; no conversational surface is configured"))
}

// ContextHasScope reports whether the handler context carries a granted scope,
// the exported face of the chat confirm path's own check (D9). The A2A handler
// needs it too: the method table grants one scope per method, so write:workflow
// at approval time is a second, finer check (FR37). With auth off no scopes
// ride the context and the answer is true.
func ContextHasScope(ctx context.Context, required string) bool {
	return hasScope(ctx, required)
}

// ContextSubject reports the verified token's subject: the access-key id for a
// key token, the IdP subject for an OAuth login. The A2A handler names a
// proposal's opener with it, since the workspace cannot separate two keys in
// one tenant (FR37). The second result is false when no token rode the call, so
// callers fall back instead of failing.
func ContextSubject(ctx context.Context) (string, bool) {
	return subjectFromContext(ctx)
}
