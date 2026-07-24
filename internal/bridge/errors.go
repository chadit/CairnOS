package bridge

import (
	"fmt"

	"github.com/chadit/CairnOS/internal/cairnerr"
)

// Canonical bridge.* error codes the Telegram bridge owns
// (.agents/spec/parity/messaging-bridge.md). They classify onto gRPC status the
// same family way the chat and engine codes do.
const (
	// CodeTelegramAPI marks a Bot API call that failed. The message carries the
	// method name and a token-redacted cause, never the raw transport error.
	CodeTelegramAPI = "bridge.telegram_api"

	// CodeSendFailed marks a turn whose delivery failed after the turn itself
	// completed. It is counted and never retried as a turn: the reply is already
	// in session history and readable from the other chat surfaces.
	CodeSendFailed = "bridge.send_failed"

	// CodeBadCallback marks a callback query whose callback_data does not parse
	// into a known confirm routing key. The confirm slice (confirm.go) is where
	// the parse happens.
	CodeBadCallback = "bridge.bad_callback"
)

// errf builds a canonical bridge error, mirroring internal/chat's local
// constructor: these codes are the bridge's own, returned as a *cairnerr.Error
// so the wire mapping stays consistent with the rest of the surface.
func errf(code, format string, args ...any) *cairnerr.Error {
	return &cairnerr.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
