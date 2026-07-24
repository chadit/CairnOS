package credentials

import "errors"

// Sentinel errors for the outbound OAuth2 credential manager. Every one carries
// a credential name and, at most, an HTTP status and a parsed OAuth error code,
// never a client secret, refresh token, or a token endpoint's raw response body.
var (
	// ErrUnknownCredential reports a Token or Has call for a name that matches no
	// configured [credential.<name>] block. It names the credential only.
	ErrUnknownCredential = errors.New("unknown credential")

	// ErrTokenEndpoint reports the token endpoint refusing the grant. It carries
	// the HTTP status code and the RFC 6749 error code parsed from the response,
	// and deliberately drops the raw response body: a broken or hostile endpoint
	// can echo the client secret or refresh token back in that body, so the body
	// must never reach an error string or a log line.
	ErrTokenEndpoint = errors.New("token endpoint refused the grant")
)
