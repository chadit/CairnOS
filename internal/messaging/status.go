package messaging

import "github.com/chadit/CairnOS/internal/api"

// The wire* funnels below adapt the api package's canonical status helpers
// (.agents/spec/04-execution-engine.md, "Mapping to gRPC status codes"), the
// same seam internal/a2a uses. Each hands back a gRPC status error, which is
// the transport contract: wrapping it would hide the gRPC code from the
// classifier, so the wrapcheck suppression lives here, in one documented
// place, rather than on every handler branch.

// wireStatus classifies a transport's send failure into the canonical gRPC
// status shape, so a messaging client sees the same codes a native API client
// does.
//
//nolint:wrapcheck // api.WireStatus returns a gRPC status error; wrapping it would mask the code
func wireStatus(err error) error {
	return api.WireStatus(err)
}

// wireInvalidArgument builds the InvalidArgument error for a request this
// package rejects before any transport call.
//
//nolint:wrapcheck // api.WireInvalidArgument returns a gRPC status error; wrapping it would mask the code
func wireInvalidArgument(format string, args ...any) error {
	return api.WireInvalidArgument(format, args...)
}

// wireIdentityNotFound builds the NotFound error for a send addressed to an
// identity the registry does not hold.
//
//nolint:wrapcheck // api.WireIdentityNotFound returns a gRPC status error; wrapping it would mask the code
func wireIdentityNotFound(name string) error {
	return api.WireIdentityNotFound(name)
}
