package bitwarden

import (
	"errors"
	"slices"

	"github.com/chadit/CairnOS/internal/cairnerr"
)

// cliOwnedSteps name the work the CLI does before the vault has any say:
// starting the daemon, waiting for its listener, and reading stored status. A
// failure at one of those is the binary's own.
//
//nolint:gochecknoglobals // a read-only classification table
var cliOwnedSteps = []string{CodeStatus, CodeServe, CodeReady}

// Unusable reports whether the CLI binary itself could not do the job, rather
// than the vault refusing what it was given. A failed login counts as the
// vault: bw exits the same way for a rejected API key and for a server it could
// not reach.
func Unusable(err error) bool {
	wrapped, ok := errors.AsType[*cairnerr.Error](err)
	if !ok {
		return false
	}

	if slices.Contains(cliOwnedSteps, wrapped.Code) {
		return true
	}

	return wrapped.Code == CodeUnlock && !answered(err)
}

// answered reports whether the daemon replied with an HTTP status. An unlock it
// answered is the vault's verdict on the master password and reads the same
// from any build, while one that got no answer is the transport failing again.
func answered(err error) bool {
	_, ok := errors.AsType[statusError](err)

	return ok
}
