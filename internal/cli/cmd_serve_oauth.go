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
// runServe so that function stays under the complexity ceiling.
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
// The two browser-facing routes mount outside the gRPC interceptor. A nil
// authenticator is a configuration error: a minted token is only useful if the
// interceptor verifies it. The concrete *auth.Authenticator is taken, not the
// minter interface, so a nil pointer cannot hide inside a non-nil interface.
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
		// one setting bounds both the access-key Login RPC and the OIDC login route.
		LoginRatePerMinute: cfg.Auth.LoginRatePerMinute,
		Logger:             logger,
	})

	return &api.Extension{
		Name:       "oauth",
		HTTPRoutes: handler.Routes(),
	}, nil
}
