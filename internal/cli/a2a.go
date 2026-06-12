package cli

import (
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"os"

	"github.com/chadit/CairnOS/internal/a2a"
	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/store"
)

// a2aGRPCScheme prefixes the endpoint a card advertises when the config gives
// no explicit endpoint_advertise; the card points at the gRPC listener.
const a2aGRPCScheme = "grpc://"

// a2aWellKnownPath echoes the public discovery path so serve can print it
// without reaching into the a2a package at the call site.
const a2aWellKnownPath = a2a.WellKnownPath

// buildA2ABundle assembles the A2A surface from the config: it resolves the
// card signing key (file, then reused auth EdDSA key, then ephemeral), signs
// the card, wires the service over the shared run manager, resolver, and
// store, and returns the bundle the serve wiring plugs into the API server. A
// nil bundle with a nil error means A2A is disabled, which keeps the whole
// protocol surface out of the process.
func buildA2ABundle(cfg *config.Config, manager *api.Manager, resolver *api.WorkflowResolver, runStore store.Store, logger *slog.Logger) (*a2a.Bundle, error) {
	if !cfg.A2A.Enabled {
		return nil, nil //nolint:nilnil // a nil bundle is the documented "a2a disabled" state, not an error
	}

	signingKey, origin, err := a2a.ResolveSigningKey(cfg.A2A.CardSigningKeyFile, reusableAuthKey(cfg), logger)
	if err != nil {
		return nil, fmt.Errorf("resolve a2a card signing key: %w", err)
	}

	logger.Info("a2a card signing key resolved", "origin", string(origin))

	endpoint := cfg.A2A.EndpointAdvertise
	if endpoint == "" {
		endpoint = a2aGRPCScheme + cfg.API.GRPC
	}

	bundle, err := a2a.NewBundle(&a2a.BundleConfig{
		NodeName:   cfg.Auth.Issuer,
		Version:    appVersion(),
		Endpoint:   endpoint,
		LoginURL:   "http://" + cfg.API.HTTP + "/v1/login",
		SigningKey: signingKey,
		Manager:    manager,
		Resolver:   resolver,
		Store:      runStore,
		TLS: a2a.TLSConfig{
			CertFile:     cfg.A2A.CertFile,
			KeyFile:      cfg.A2A.KeyFile,
			ClientCAFile: cfg.A2A.ClientCAFile,
		},
		Logger: logger,
	})
	if err != nil {
		return nil, fmt.Errorf("build a2a bundle: %w", err)
	}

	return bundle, nil
}

// reusableAuthKey returns the auth layer's EdDSA private key when the auth tier
// is asymmetric, so the card and the cross-org tokens share one trust anchor
// (.agents/spec/08-auth-and-access.md, Change 3). It returns nil when auth is
// off or running HS256, which sends ResolveSigningKey to its file or ephemeral
// path instead. A load failure also yields nil: a failure here is not fatal to
// A2A, since the file and ephemeral fallbacks still produce a usable key.
func reusableAuthKey(cfg *config.Config) ed25519.PrivateKey {
	if !cfg.Auth.Enabled || cfg.Auth.SigningAlgorithm != auth.AlgEdDSA {
		return nil
	}

	material, err := auth.LoadMaterial(cfg.Auth.SigningAlgorithm, os.Getenv)
	if err != nil {
		return nil
	}

	key, _, ok := material.EdDSAKeys()
	if !ok {
		return nil
	}

	return key
}
