package reauth

import "errors"

// ErrUnknownProvider reports a probe for a provider the package has no reader for.
var ErrUnknownProvider = errors.New("reauth: unknown provider")

// ErrNoCredential reports that the provider's credential file does not exist, so
// the caller can tell "never signed in" from a parse failure.
var ErrNoCredential = errors.New("reauth: no credential file")

// ErrMalformedJWT reports a token that is not a three-part JWT, so the expiry
// cannot be decoded.
var ErrMalformedJWT = errors.New("reauth: malformed jwt")
