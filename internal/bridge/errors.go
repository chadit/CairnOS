package bridge

import (
	"fmt"

	"github.com/chadit/CairnOS/internal/cairnerr"
)

// CodeSendFailed marks a turn whose delivery failed after the turn itself
// completed. It is counted and never retried as a turn: the reply is already in
// session history and readable from the other chat surfaces. Transport-specific
// codes live with their adapter; this one is the core's because the core is what
// decides not to retry.
const CodeSendFailed = "bridge.send_failed"

// errf builds a canonical bridge error, mirroring internal/chat's local
// constructor: these codes are the bridge's own, returned as a *cairnerr.Error
// so the wire mapping stays consistent with the rest of the surface.
func errf(code, format string, args ...any) *cairnerr.Error {
	return &cairnerr.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
