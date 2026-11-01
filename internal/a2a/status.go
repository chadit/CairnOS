package a2a

import (
	"google.golang.org/grpc/status"

	"github.com/chadit/CairnOS/internal/api"
)

// The wire* funnels below adapt the api package's canonical status helpers.
// Each hands back a gRPC status error, which is the transport contract:
// wrapping it would hide the gRPC code from the classifier, so the wrapcheck
// suppression lives here, in one documented place, rather than scattered across
// the service handlers.

// wirePassthrough returns an error the api package already shaped as a gRPC
// status (the workflow resolver's NotFound). Re-wrapping or re-classifying it
// would change its code, so it passes through untouched. Returning a local
// function's result, rather than the external resolver's directly, is what
// keeps the call site clear of the wrapcheck rule.
func wirePassthrough(err error) error {
	return err
}

// wireStatus classifies any error into the canonical gRPC status shape.
//
//nolint:wrapcheck // api.WireStatus returns a gRPC status error; wrapping it would mask the code
func wireStatus(err error) error {
	return api.WireStatus(err)
}

// wireRunLookup classifies a store GetRun failure, mapping a missing run to
// NotFound.
//
//nolint:wrapcheck // api.WireRunLookup returns a gRPC status error; wrapping it would mask the code
func wireRunLookup(runID string, err error) error {
	return api.WireRunLookup(runID, err)
}

// wireRunFinished builds the FailedPrecondition error for a cancel that
// targets an already-terminal run.
//
//nolint:wrapcheck // api.WireRunFinished returns a gRPC status error; wrapping it would mask the code
func wireRunFinished(runID, runStatus string) error {
	return api.WireRunFinished(runID, runStatus)
}

// wireRunNotManaged builds the FailedPrecondition error for a cancel that
// targets a run this node did not start.
//
//nolint:wrapcheck // api.WireRunNotManaged returns a gRPC status error; wrapping it would mask the code
func wireRunNotManaged(runID string) error {
	return api.WireRunNotManaged(runID)
}

// wireInvalidArgument builds the InvalidArgument error for a request this
// adapter rejects before any work starts.
//
//nolint:wrapcheck // api.WireInvalidArgument returns a gRPC status error; wrapping it would mask the code
func wireInvalidArgument(format string, args ...any) error {
	return api.WireInvalidArgument(format, args...)
}

// wireChatDisabled builds the FailedPrecondition error for a conversational
// request on a node with no chat surface configured.
//
//nolint:wrapcheck // api.WireChatDisabled returns a gRPC status error; wrapping it would mask the code
func wireChatDisabled() error {
	return api.WireChatDisabled()
}

// wireContext maps a canceled or expired request context onto its gRPC code,
// so a peer whose deadline lapsed mid-turn reads DeadlineExceeded rather than
// an internal error.
//
//nolint:wrapcheck // status.FromContextError yields the gRPC status error itself; wrapping it would mask the code
func wireContext(err error) error {
	return status.FromContextError(err).Err()
}
