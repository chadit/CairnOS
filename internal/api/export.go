package api

import (
	"google.golang.org/grpc/codes"

	"github.com/chadit/CairnOS/internal/cairnerr"
)

// The exports below are the surface internal/a2a builds on. A2A is a protocol
// adapter over the same run lifecycle this package owns
// (.agents/spec/09-a2a-and-hive.md, "Core methods"), so it reuses the run
// manager, the workflow resolver, and this one error-mapping path rather than
// duplicating the 04 status table. Keeping the export list small and in one
// file documents the seam between the two packages. The names carry a Wire
// suffix to stay clear of the package-private helpers they delegate to.

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
