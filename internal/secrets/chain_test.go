package secrets_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/secrets"
)

const (
	staleEnvValue     = "stale-env-value"
	rotatedStoreValue = "rotated-store-value"
)

// errStoreOutage stands in for any non-ErrNotFound failure a source can return
// (a store outage, a bad key). The chain must surface it immediately instead of
// falling through.
var errStoreOutage = errors.New("store outage")

// fakeSource is a map-backed secret source the chain tests drive. It counts Get
// calls so a test can prove a later source was never consulted.
type fakeSource struct {
	values map[string]string
	err    error
	calls  int
}

// Get returns the configured error, the mapped value, or ErrNotFound. Names are
// normalized so the fake agrees with the env source on identity.
func (f *fakeSource) Get(_ context.Context, name string) (string, error) {
	f.calls++

	if f.err != nil {
		return "", f.err
	}

	value, ok := f.values[secrets.NormalizeName(name)]
	if !ok {
		return "", secrets.ErrNotFound
	}

	return value, nil
}

func TestChainFallsThroughOnNotFound(t *testing.T) {
	t.Parallel()

	empty := &fakeSource{values: map[string]string{}}
	backing := &fakeSource{values: map[string]string{testName: testValue}}

	value, err := secrets.Chain(empty, backing).Get(t.Context(), testName)
	if err != nil {
		t.Fatalf("chain get: %v", err)
	}

	if value != testValue {
		t.Errorf("chain value = %q, want %q", value, testValue)
	}
}

// TestChainHardStopsOnOtherError proves the deliberate hard stop: a store outage
// returns immediately and the chain never falls back to a later source, so a
// broken store cannot silently mask a value.
func TestChainHardStopsOnOtherError(t *testing.T) {
	t.Parallel()

	broken := &fakeSource{err: errStoreOutage}
	backstop := &fakeSource{values: map[string]string{testName: testValue}}

	_, err := secrets.Chain(broken, backstop).Get(t.Context(), testName)
	if !errors.Is(err, errStoreOutage) {
		t.Fatalf("chain error = %v, want the store outage", err)
	}

	if backstop.calls != 0 {
		t.Errorf("backstop consulted %d times after a hard stop, want 0", backstop.calls)
	}
}

func TestChainAllMissWrapsNotFound(t *testing.T) {
	t.Parallel()

	one := &fakeSource{values: map[string]string{}}
	two := &fakeSource{values: map[string]string{}}

	_, err := secrets.Chain(one, two).Get(t.Context(), testName)
	if !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("all-miss error = %v, want ErrNotFound", err)
	}

	if got := err.Error(); !strings.Contains(got, testName) {
		t.Errorf("all-miss error %q does not name the secret", got)
	}
}

// TestChainEnvShadowsStore is the security-council amendment 1 hazard: env stays
// first, so a stale env var silently shadows a rotated store value. A rotation
// can look done while the old secret is still served. The chain resolution here
// proves the hazard; the wiring surfaces it (startup warning) and the CLI marks
// it in ls, both in a later slice.
func TestChainEnvShadowsStore(t *testing.T) {
	t.Setenv(secrets.EnvPrefix+"DB_PASSWORD", staleEnvValue)

	rotatedStore := &fakeSource{values: map[string]string{testName: rotatedStoreValue}}

	value, err := secrets.Chain(secrets.NewEnv(), rotatedStore).Get(t.Context(), testName)
	if err != nil {
		t.Fatalf("chain get: %v", err)
	}

	if value != staleEnvValue {
		t.Fatalf("chain value = %q, want the stale env value %q (env must win)", value, staleEnvValue)
	}

	if rotatedStore.calls != 0 {
		t.Errorf("store consulted %d times though env shadowed it, want 0", rotatedStore.calls)
	}
}
