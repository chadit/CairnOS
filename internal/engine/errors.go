package engine

import (
	"errors"
	"fmt"

	"github.com/chadit/CairnOS/internal/cairnerr"
)

// Sentinel errors for the engine package. Run and action failures carry
// canonical cairnerr codes instead; these sentinels cover the argument and
// configuration checks underneath that machinery.
var (
	// ErrNilWorkflow reports RunWorkflow called without a parsed workflow.
	ErrNilWorkflow = errors.New("nil workflow")

	// ErrNoSecretSource reports a secrets reference with no secret source
	// configured on the engine.
	ErrNoSecretSource = errors.New("no secret source configured")

	// ErrNonScalarEmbed reports a reference embedded in surrounding text that
	// resolved to an object or a list; only scalars render into strings.
	ErrNonScalarEmbed = errors.New("embedded reference resolves to a non-scalar value")
)

// errf builds a canonical engine error. A local constructor instead of
// cairnerr.Newf keeps wrapcheck satisfied where these errors return
// unwrapped: the codes are the engine's own surface, not a foreign failure
// that needs wrapping context.
func errf(code, format string, args ...any) *cairnerr.Error {
	return &cairnerr.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
