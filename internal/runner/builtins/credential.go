package builtins

import (
	"context"
	"fmt"
	"net/http"
)

// cfgKeyCredential names the optional [runner.<name>] config key that binds an
// http or mcp_client runner to a [credential.<name>] block. When set, the
// runner sends Authorization: Bearer with a token minted per request, unless an
// explicit Authorization header is already present (default_headers or a
// per-call header wins).
const cfgKeyCredential = "credential"

// headerAuthorization is the request header a credential bearer token sets, and
// the one an explicit config or per-call header overrides.
const headerAuthorization = "Authorization"

// TokenLookup mints a bearer token for a named outbound credential. The
// internal/credentials Manager satisfies it structurally, so this package never
// imports internal/credentials: the runner side depends only on the two methods
// it calls, Token per request and Has at build time.
type TokenLookup interface {
	// Token returns a fresh, cached, or refreshed access token for name. The
	// value is a secret; callers set it on a request header and never place it
	// in a log line or error string.
	Token(ctx context.Context, name string) (string, error)
	// Has reports whether name matches a configured credential, so a factory
	// rejects an unknown reference at build time (registry BuildAll) rather than
	// on the first request.
	Has(name string) bool
}

// requireCredential resolves a runner's credential config against the wired
// lookup at build time. An empty key is the no-credential path and returns "".
// A named credential with a nil lookup (no [credential.*] blocks wired) or an
// unknown name is a config error, so a typo or a missing manager fails BuildAll
// at startup rather than mid-run. The error names the credential, never a
// secret value.
func requireCredential(cfg map[string]any, lookup TokenLookup) (string, error) {
	name, err := configValue(cfg, cfgKeyCredential, "")
	if err != nil {
		return "", err
	}

	if name == "" {
		return "", nil
	}

	if lookup == nil {
		return "", fmt.Errorf("%w: credential %q referenced but no credentials are configured", ErrConfigValue, name)
	}

	if !lookup.Has(name) {
		return "", fmt.Errorf("%w: credential %q is not a configured [credential.<name>] block", ErrConfigValue, name)
	}

	return name, nil
}

// setBearerIfAbsent sets Authorization: Bearer <token> on request when no
// Authorization header is already present, so a default_headers or per-call
// header wins over the credential. It mints the token through lookup with ctx.
// A mint failure returns the lookup's error unchanged; that error carries a
// credential name and status only, never the token or the client secret.
func setBearerIfAbsent(ctx context.Context, request *http.Request, lookup TokenLookup, credential string) error {
	if credential == "" || lookup == nil {
		return nil
	}

	if request.Header.Get(headerAuthorization) != "" {
		return nil
	}

	token, err := lookup.Token(ctx, credential)
	if err != nil {
		return fmt.Errorf("mint token for credential %q: %w", credential, err)
	}

	request.Header.Set(headerAuthorization, "Bearer "+token)

	return nil
}

// bearerRoundTripper injects the credential's bearer token into every request
// the mcp_client streamable HTTP transport makes, unless the request already
// carries an Authorization header. It clones the request before mutating it so
// the SDK's own request is untouched (the RoundTripper contract), and mints the
// token under the request's own context. The token is a secret: it rides the
// header only, never a log line or error.
type bearerRoundTripper struct {
	base       http.RoundTripper
	lookup     TokenLookup
	credential string
}

// RoundTrip implements http.RoundTripper.
func (b *bearerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())

	if err := setBearerIfAbsent(request.Context(), cloned, b.lookup, b.credential); err != nil {
		return nil, err
	}

	response, err := b.base.RoundTrip(cloned)
	if err != nil {
		return nil, fmt.Errorf("credential bearer transport: %w", err)
	}

	return response, nil
}
