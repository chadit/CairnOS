package telegram

import (
	"fmt"

	"github.com/chadit/CairnOS/internal/cairnerr"
)

// Canonical error codes the Telegram adapter owns. They keep the bridge.* prefix
// because they classify onto gRPC status in the same family as the core's
// bridge.send_failed, and renaming them would change an operator-visible code for
// no behavior gain.
const (
	// CodeTelegramAPI marks a Bot API call that failed. The message carries the
	// method name and a token-redacted cause, never the raw transport error.
	CodeTelegramAPI = "bridge.telegram_api"

	// CodeBadCallback marks a callback query whose callback_data does not parse
	// into a known confirm routing key.
	CodeBadCallback = "bridge.bad_callback"
)

// errf builds a canonical error, mirroring the core's and internal/chat's local
// constructors: these codes are the adapter's own, returned as a *cairnerr.Error
// so the wire mapping stays consistent with the rest of the surface.
func errf(code, format string, args ...any) *cairnerr.Error {
	return &cairnerr.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
