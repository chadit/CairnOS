package secrets

import "errors"

// ErrNotFound reports a referenced secret with no value in the source. The
// engine surfaces it as the canonical config.missing_secret error.
var ErrNotFound = errors.New("secret not found")
