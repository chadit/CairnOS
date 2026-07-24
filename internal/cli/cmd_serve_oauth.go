package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/oauth"
	"github.com/chadit/CairnOS/internal/telemetry"
)

// attachOAuthServe mounts the OIDC login extension when [oauth] is enabled,
// appending it to apiCfg and printing the login banner. It returns the failure
// exit code and false only when the extension cannot be built (telemetry
// flushed); a disabled config mounts nothing and returns true. Kept out of
// runServe so that function stays under the complexity ceiling, the same shape
// as attachMCPServe and attachWebhookServe.
func (a *App) attachOAuthServe(ctx context.Context, cfg *config.Config, tel *telemetry.Telemetry, authn *auth.Authenticator, members oauth.Tenancy, apiCfg *api.Config, logger *slog.Logger) (int, bool) {
	if !cfg.OAuth.Enabled {
		return ExitOK, true
	}

	ext, err := buildOAuthExtension(ctx, cfg, authn, members, logger)
	if err != nil {
		return a.failServe(ctx, tel, "oauth", err), false
	}

	apiCfg.Extensions = append(apiCfg.Extensions, ext)

	a.printOutf("oauth login available at %s%s\n", "http://"+cfg.API.HTTP, oauthloginPath)

	return ExitOK, true
}

// oauthloginPath echoes the handler's login route for the serve banner.
const oauthloginPath = oauth.LoginPath

// buildOAuthExtension wires the OIDC human-login handler into an api.Extension.
// It discovers the IdP through its issuer (fetching the discovery document and
// JWKS URL), builds the oauth2 config and the ID-token verifier, and mounts the
// two browser-facing routes outside the gRPC interceptor. OAuth needs auth on:
// a minted token is only useful if the interceptor verifies it, so a nil
// authenticator is a configuration error here. The concrete *auth.Authenticator
// is taken rather than the minter interface so the nil check is real (a nil
// pointer in an interface reads as non-nil). ctx bounds the discovery fetch.
func buildOAuthExtension(ctx context.Context, cfg *config.Config, authn *auth.Authenticator, members oauth.Tenancy, logger *slog.Logger) (*api.Extension, error) {
	if authn == nil {
		return nil, fmt.Errorf("%w: oauth login requires auth.enabled = true so the minted token can be verified", errOAuthNeedsAuth)
	}

	provider, err := oidc.NewProvider(ctx, cfg.OAuth.Issuer)
	if err != nil {
		return nil, fmt.Errorf("discover oauth issuer %q: %w", cfg.OAuth.Issuer, err)
	}

	oauth2Cfg := &oauth2.Config{
		ClientID:     cfg.OAuth.ClientID,
		ClientSecret: cfg.OAuth.ClientSecret,
		RedirectURL:  cfg.OAuth.RedirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       []string{oidc.ScopeOpenID, "email"},
	}

	verifier := provider.Verifier(&oidc.Config{ClientID: cfg.OAuth.ClientID})

	handler := oauth.New(&oauth.Config{
		Verifier:         verifier,
		Exchanger:        oauth2Cfg,
		Minter:           authn,
		Members:          members,
		DefaultWorkspace: cfg.OAuth.DefaultWorkspace,
		DefaultRole:      cfg.OAuth.DefaultRole,
		Provision: oauth.ProvisionPolicy{
			Enabled:             cfg.OAuth.AutoProvision,
			AllowedEmailDomains: cfg.OAuth.AllowedEmailDomains,
			AllowedSubjects:     cfg.OAuth.AllowedSubjects,
		},
		// The unauthenticated login route reuses the auth login-rate budget so
		// one knob bounds both the access-key Login RPC and the OIDC login route
		// (spec L1).
		LoginRatePerMinute: cfg.Auth.LoginRatePerMinute,
		Logger:             logger,
	})

	return &api.Extension{
		Name:       "oauth",
		HTTPRoutes: handler.Routes(),
	}, nil
}
