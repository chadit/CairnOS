// Package credentials mints outbound OAuth2 access tokens for the http and
// mcp_client runners, so a workflow calls a SaaS API without hand-managed
// tokens. It is the outbound third-party counterpart to internal/oauth, which
// is the inbound OIDC human-login path.
//
// One TokenSource is built per [credential.<name>] block over golang.org/x/oauth2:
// the client_credentials grant uses clientcredentials.Config, the refresh_token
// grant uses oauth2.Config with a seeded refresh token. Both come reuse-wrapped
// by the library, which is the token cache with expiry, so there is no
// hand-rolled cache. Client secrets and refresh tokens are resolved through the
// secret chain once, at construction; a value never rides an error.
//
// Every token mint and refresh rides the shared egress guard. The manager injects
// a guarded HTTP client through the oauth2.HTTPClient context key on each
// TokenSource's base context, so loopback, link-local, and private targets are
// refused at dial (after DNS resolution) unless the credential opts in with
// allow_private_targets.
//
// Context capture, stated because it is surprising: x/oauth2 TokenSources capture
// the context they are built with and refresh under it forever, and
// TokenSource.Token() takes no context. The base context is
// context.WithValue(context.WithoutCancel(wiringCtx), oauth2.HTTPClient, client),
// so a refresh triggered late in the process is not killed by a parent
// cancellation mid-mint, and the refresh is bounded only by the guarded client's
// timeout. Token(ctx, name) checks ctx at entry and returns the caller's error
// without touching the source when the caller is already done; past that point a
// per-call ctx does not bound the refresh. A caller with a short deadline can
// therefore get a token back after its own deadline expired: the alternative,
// aborting a shared refresh because one caller was impatient, would poison the
// cached source for every other caller.
package credentials

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/chadit/CairnOS/internal/config"
)

// SecretResolver is the slice of the secret chain the manager needs: it resolves
// a credential's client_secret_ref and refresh_token_ref to their values. The
// engine's secret source (env alone, or Chain(env, store)) satisfies it
// structurally, so the manager never imports internal/secrets.
type SecretResolver interface {
	Get(ctx context.Context, name string) (string, error)
}

// Manager holds one reuse-wrapped TokenSource per configured credential. It is
// built once at wiring time and shared read-only across runs.
type Manager struct {
	sources map[string]oauth2.TokenSource
	names   []string
}

// New resolves each credential's secret refs through source, builds one
// TokenSource per name, and injects the guarded client from newClient into each
// source's base context. Construction does no network: a missing ref fails here,
// naming the credential and the ref, so a broken credential fails at startup
// rather than mid-run. cfgs are assumed already validated by config.Load.
func New(
	ctx context.Context,
	cfgs map[string]config.Credential,
	source SecretResolver,
	newClient func(allowPrivate bool) *http.Client,
) (*Manager, error) {
	manager := &Manager{
		sources: make(map[string]oauth2.TokenSource, len(cfgs)),
		names:   slices.Sorted(maps.Keys(cfgs)),
	}

	for _, name := range manager.names {
		cred := cfgs[name]

		if err := manager.addSource(ctx, name, &cred, source, newClient); err != nil {
			return nil, err
		}
	}

	return manager, nil
}

// refreshConfig builds the refresh_token grant's config and the token that
// seeds it: the refresh token is required, the client secret is optional (some
// providers want it on the refresh call).
func refreshConfig(
	ctx context.Context,
	name string,
	cred *config.Credential,
	source SecretResolver,
) (*oauth2.Config, *oauth2.Token, error) {
	refresh, err := source.Get(ctx, cred.RefreshTokenRef)
	if err != nil {
		return nil, nil, fmt.Errorf("credential %q: resolve refresh_token_ref %q: %w", name, cred.RefreshTokenRef, err)
	}

	conf := &oauth2.Config{
		ClientID: cred.ClientID,
		Endpoint: oauth2.Endpoint{TokenURL: cred.TokenURL},
	}

	if cred.ClientSecretRef != "" {
		secret, secErr := source.Get(ctx, cred.ClientSecretRef)
		if secErr != nil {
			return nil, nil, fmt.Errorf("credential %q: resolve client_secret_ref %q: %w", name, cred.ClientSecretRef, secErr)
		}

		conf.ClientSecret = secret
	}

	return conf, &oauth2.Token{RefreshToken: refresh}, nil
}

// Token returns a fresh, cached, or refreshed access token for name. A caller
// whose ctx is already done gets its own error back without touching the shared
// source, so one impatient caller never triggers or poisons a refresh others
// depend on. An unknown name is ErrUnknownCredential; a token endpoint refusal is
// ErrTokenEndpoint carrying only the status and parsed error code.
func (m *Manager) Token(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("credential %q: %w", name, err)
	}

	tokenSource, ok := m.sources[name]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownCredential, name)
	}

	token, err := tokenSource.Token()
	if err != nil {
		return "", mapTokenError(name, err)
	}

	return token.AccessToken, nil
}

// Has reports whether name matches a configured credential. Factories call it at
// build time so an unknown reference fails at startup, not mid-run.
func (m *Manager) Has(name string) bool {
	_, ok := m.sources[name]

	return ok
}

// Names returns the configured credential names in sorted order. It returns a
// copy so a caller cannot mutate the manager's state.
func (m *Manager) Names() []string {
	return slices.Clone(m.names)
}

// addSource resolves cred's secret refs and stores the reuse-wrapped
// TokenSource for its grant under name. The base context carries the guarded
// client and drops the caller's cancellation so a late refresh survives (see
// the package doc's context-capture note).
func (m *Manager) addSource(
	ctx context.Context,
	name string,
	cred *config.Credential,
	source SecretResolver,
	newClient func(allowPrivate bool) *http.Client,
) error {
	base := context.WithValue(context.WithoutCancel(ctx), oauth2.HTTPClient, newClient(cred.AllowPrivateTargets))

	switch cred.Grant {
	case config.GrantClientCredentials:
		secret, err := source.Get(ctx, cred.ClientSecretRef)
		if err != nil {
			return fmt.Errorf("credential %q: resolve client_secret_ref %q: %w", name, cred.ClientSecretRef, err)
		}

		conf := &clientcredentials.Config{
			ClientID:     cred.ClientID,
			ClientSecret: secret,
			TokenURL:     cred.TokenURL,
			Scopes:       cred.Scopes,
		}

		m.sources[name] = conf.TokenSource(base)

		return nil

	case config.GrantRefreshToken:
		conf, seed, err := refreshConfig(ctx, name, cred, source)
		if err != nil {
			return err
		}

		m.sources[name] = conf.TokenSource(base, seed)

		return nil

	default:
		// Unreachable: config.Load rejects any other grant. Defensive so a future
		// grant added to the config without a case here fails loudly.
		return fmt.Errorf("%w: credential.%s.grant %q", config.ErrInvalidConfig, name, cred.Grant)
	}
}

// mapTokenError converts a TokenSource failure into a safe error. A token
// endpoint refusal surfaces as ErrTokenEndpoint with only the HTTP status and
// RFC 6749 code; the raw body is dropped so a hostile endpoint cannot leak an
// echoed client secret into a log. Any other failure carries no request body,
// so wrapping it is safe.
func mapTokenError(name string, err error) error {
	if retrieve, ok := errors.AsType[*oauth2.RetrieveError](err); ok {
		var status int
		if retrieve.Response != nil {
			status = retrieve.Response.StatusCode
		}

		return fmt.Errorf("%w: credential %q, status %d, error %q", ErrTokenEndpoint, name, status, retrieve.ErrorCode)
	}

	return fmt.Errorf("token for credential %q: %w", name, err)
}
