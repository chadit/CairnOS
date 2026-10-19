package builtins_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Shared fixtures for the credential (bearer token) tests across the http and
// mcp_client runners.
const (
	credentialKey     = "credential"
	defaultHeadersKey = "default_headers"
	authHeader        = "Authorization"
	bearerPrefix      = "Bearer "
	credName          = "saas_api"
	credToken         = "s3cr3t-bearer-value-xyz" //nolint:gosec // test fixture, not a real credential
	unknownCred       = "no_such_cred"
)

// fakeTokenLookup is a test TokenLookup. It mints a fixed token per known
// credential name, counts Token calls, and can be set to fail, so runner tests
// drive bearer injection, header precedence, unknown-credential rejection, and
// the mint-failure path without a live OAuth2 endpoint. The internal Manager
// satisfies the same interface, so these tests exercise the real wiring seam.
type fakeTokenLookup struct {
	tokens map[string]string
	err    error

	mu    sync.Mutex
	calls int
}

// Token implements builtins.TokenLookup.
func (f *fakeTokenLookup) Token(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()

	if f.err != nil {
		return "", f.err
	}

	token, ok := f.tokens[name]
	if !ok {
		return "", fmt.Errorf("fake: no token for %q", name)
	}

	return token, nil
}

// Has implements builtins.TokenLookup.
func (f *fakeTokenLookup) Has(name string) bool {
	_, ok := f.tokens[name]

	return ok
}

// callCount reports how many times Token was minted, so a test can assert a
// credential was consulted (or, for the header-precedence case, was not).
func (f *fakeTokenLookup) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

// buildThroughRegistry registers factory under name and builds it with cfg
// through BuildAll, the path a real startup takes, so a credential rejection
// surfaces exactly where the runner one would.
func buildThroughRegistry(t *testing.T, name string, factory runner.Factory, cfg map[string]any) error {
	t.Helper()

	registry := runner.NewRegistry()
	if err := registry.Register(name, factory); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}

	if err := registry.BuildAll(map[string]map[string]any{name: cfg}); err != nil {
		return fmt.Errorf("build all: %w", err)
	}

	return nil
}

// TestCredentialBuildRejectsUnknownAndUnwired proves both credential-aware
// factories fail at registry BuildAll when a runner names a credential the
// lookup does not know, or when no lookup is wired at all. A typo or a missing
// manager must surface at startup, not on the first request.
func TestCredentialBuildRejectsUnknownAndUnwired(t *testing.T) {
	t.Parallel()

	known := &fakeTokenLookup{tokens: map[string]string{credName: credToken}}

	cases := []struct {
		factory runner.Factory
		cfg     map[string]any
		name    string
		runner  string
	}{
		{
			name:    "http unknown credential",
			runner:  builtins.HTTPName,
			factory: builtins.HTTPFactoryWithCredentials(known),
			cfg:     map[string]any{credentialKey: unknownCred},
		},
		{
			name:    "http credential with no lookup wired",
			runner:  builtins.HTTPName,
			factory: builtins.HTTPFactoryWithCredentials(nil),
			cfg:     map[string]any{credentialKey: credName},
		},
		{
			name:    "mcp_client unknown credential",
			runner:  builtins.MCPClientName,
			factory: builtins.MCPClientFactoryWithCredentials(known, nil),
			cfg:     map[string]any{serverURLKey: exampleMCP, toolKey: echoTool, credentialKey: unknownCred, allowPrivateKey: true},
		},
		{
			name:    "mcp_client credential with no lookup wired",
			runner:  builtins.MCPClientName,
			factory: builtins.MCPClientFactoryWithCredentials(nil, nil),
			cfg:     map[string]any{serverURLKey: exampleMCP, toolKey: echoTool, credentialKey: credName, allowPrivateKey: true},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := buildThroughRegistry(t, tt.runner, tt.factory, tt.cfg)
			if !errors.Is(err, builtins.ErrConfigValue) {
				t.Fatalf("BuildAll error = %v, want ErrConfigValue", err)
			}
		})
	}
}
