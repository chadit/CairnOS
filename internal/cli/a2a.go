package cli

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"

	"github.com/chadit/CairnOS/internal/a2a"
	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/bridge"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/messaging"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/telemetry"
)

// a2aServeDeps groups the seams attachA2AServe wires the A2A bundle over, so the
// helper takes one argument rather than a long parameter list (gocritic
// paramTypeCombine, varnamelen on a wide signature).
type a2aServeDeps struct {
	manager *api.Manager
	store   store.Store

	// core is the shared bridge core a peer's SendMessage runs its turn through.
	// It is nil when [chat] is off, which is what makes SendMessage answer
	// FailedPrecondition instead of pretending a turn ran.
	core *bridge.Core

	// identities is the registry the card's channel list is rendered from. It is
	// read here, not earlier: the Mattermost builder registers into it just
	// above this call and the card is signed at NewBundle.
	identities *messaging.Registry

	logger       *slog.Logger
	workflowsDir string
}

// attachA2AServe builds the A2A bundle and, when enabled, mounts it and prints
// the card banner. It returns the failure exit code and false only when the
// bundle cannot be built or attached; a disabled A2A mounts nothing and returns
// true. Kept out of runServe for the same reason attachMCPServe is.
func (a *App) attachA2AServe(ctx context.Context, cfg *config.Config, tel *telemetry.Telemetry, apiCfg *api.Config, deps *a2aServeDeps) (int, bool) {
	bundle, err := buildA2ABundle(cfg, api.NewWorkflowResolver(deps.workflowsDir, deps.logger), a.envLookup(), deps)
	if err != nil {
		return a.failServe(ctx, tel, "a2a", err), false
	}

	if bundle == nil {
		return ExitOK, true
	}

	if err := bundle.Attach(apiCfg); err != nil {
		return a.failServe(ctx, tel, "a2a", err), false
	}

	a.printOutf("a2a card published at %s%s\n", "http://"+cfg.API.HTTP, a2aWellKnownPath)

	return ExitOK, true
}

// a2aGRPCScheme prefixes the endpoint a card advertises when the config gives
// no explicit endpoint_advertise; the card points at the gRPC listener.
const a2aGRPCScheme = "grpc://"

// a2aWellKnownPath echoes the public discovery path so serve can print it
// without reaching into the a2a package at the call site.
const a2aWellKnownPath = a2a.WellKnownPath

// buildA2ABundle assembles the A2A surface: it resolves the card signing key
// (file, then the reused auth EdDSA key, then ephemeral), signs the card over
// the advertised endpoint and channels, and wires the service over the shared
// manager, resolver, store, and bridge core. A nil bundle with a nil error
// means A2A is disabled.
func buildA2ABundle(
	cfg *config.Config, resolver *api.WorkflowResolver, lookup environ.Lookup, deps *a2aServeDeps,
) (*a2a.Bundle, error) {
	logger := deps.logger

	if !cfg.A2A.Enabled {
		return nil, nil //nolint:nilnil // a nil bundle is the documented "a2a disabled" state, not an error
	}

	signingKey, origin, err := a2a.ResolveSigningKey(cfg.A2A.CardSigningKeyFile, reusableAuthKey(cfg, lookup), logger)
	if err != nil {
		return nil, fmt.Errorf("resolve a2a card signing key: %w", err)
	}

	logger.Info("a2a card signing key resolved", "origin", string(origin))

	endpoint := cfg.A2A.EndpointAdvertise
	if endpoint == "" {
		endpoint = a2aGRPCScheme + cfg.API.GRPC
	}

	bundleCfg := &a2a.BundleConfig{
		NodeName:   cfg.Auth.Issuer,
		Version:    appVersion(),
		Endpoint:   endpoint,
		LoginURL:   "http://" + cfg.API.HTTP + "/v1/login",
		SigningKey: signingKey,
		Channels:   a2aChannels(deps.identities),
		Manager:    deps.manager,
		Resolver:   resolver,
		Store:      deps.store,
		TLS: a2a.TLSConfig{
			CertFile:     cfg.A2A.CertFile,
			KeyFile:      cfg.A2A.KeyFile,
			ClientCAFile: cfg.A2A.ClientCAFile,
		},
		Logger: logger,
	}

	// Assigned only when non-nil: a typed-nil *bridge.Core inside the interface
	// field reads as "chat on" to the service and would defeat the nil check
	// that answers chat-disabled.
	if deps.core != nil {
		bundleCfg.Bridge = deps.core
	}

	bundle, err := a2a.NewBundle(bundleCfg)
	if err != nil {
		return nil, fmt.Errorf("build a2a bundle: %w", err)
	}

	return bundle, nil
}

// a2aChannels renders the registry's identities as card channels, so a peer
// reading the card learns which chat accounts this node answers on. An
// empty or nil registry yields nothing, which the card omits.
func a2aChannels(registry *messaging.Registry) []a2a.Channel {
	if registry == nil {
		return nil
	}

	identities := registry.List()

	channels := make([]a2a.Channel, 0, len(identities))
	for _, identity := range identities {
		channels = append(channels, a2a.Channel{
			Type:        identity.Transport,
			URL:         identity.URL,
			Username:    identity.Username,
			DisplayName: identity.DisplayName,
		})
	}

	return channels
}

// reusableAuthKey returns the auth layer's EdDSA private key when the auth
// tier is asymmetric, so the card and the cross-org tokens share one trust
// anchor. It returns nil when auth is off, when auth runs HS256, or when the
// material fails to load, which sends ResolveSigningKey to its file or
// ephemeral path instead.
func reusableAuthKey(cfg *config.Config, lookup environ.Lookup) ed25519.PrivateKey {
	if !cfg.Auth.Enabled || cfg.Auth.SigningAlgorithm != auth.AlgEdDSA {
		return nil
	}

	material, err := auth.LoadMaterial(cfg.Auth.SigningAlgorithm, lookup.Get)
	if err != nil {
		return nil
	}

	key, _, ok := material.EdDSAKeys()
	if !ok {
		return nil
	}

	return key
}
