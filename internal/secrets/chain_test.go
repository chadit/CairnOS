package secrets_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/secrets"
)

// envSource is the env half of a shadowing chain, holding the one variable the
// case is about. The value is staged here rather than exported so the two
// shadow cases can run beside each other.
func envSource(value string) *secrets.Env {
	return secrets.NewEnvWith(environ.Map(map[string]string{secrets.EnvPrefix + "DB_PASSWORD": value}))
}

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
		return "", fmt.Errorf("%w: %s (fake store)", secrets.ErrNotFound, name)
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

// emptyEnv is an env source with nothing set, so every lookup misses with its
// "(set CAIRNOS_SECRET_<NAME>)" hint.
func emptyEnv() *secrets.Env {
	return secrets.NewEnvWith(environ.Map(map[string]string{}))
}

// TestChainAllMissJoinsEverySourcesMiss proves an all-miss keeps each source's
// own error, the env hint included, and still matches ErrNotFound.
func TestChainAllMissJoinsEverySourcesMiss(t *testing.T) {
	t.Parallel()

	store := &fakeSource{values: map[string]string{}}

	_, err := secrets.Chain(emptyEnv(), store).Get(t.Context(), testName)
	if !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("all-miss error = %v, want ErrNotFound", err)
	}

	got := err.Error()
	for _, want := range []string{"(set " + secrets.EnvPrefix + "DB_PASSWORD)", "(fake store)"} {
		if !strings.Contains(got, want) {
			t.Errorf("all-miss error %q is missing a source's own miss %q", got, want)
		}
	}
}

// TestChainOfOnlyEnvKeepsTheEnvMiss proves a chain around Env alone misses with
// exactly Env's error, so wrapping Env in a chain costs the operator no hint.
func TestChainOfOnlyEnvKeepsTheEnvMiss(t *testing.T) {
	t.Parallel()

	_, envErr := emptyEnv().Get(t.Context(), testName)
	_, chainErr := secrets.Chain(emptyEnv()).Get(t.Context(), testName)

	if envErr == nil || chainErr == nil {
		t.Fatalf("env error = %v, chain error = %v, want both to miss", envErr, chainErr)
	}

	if chainErr.Error() != envErr.Error() {
		t.Errorf("chain error = %q, want the env error %q", chainErr, envErr)
	}
}

// TestChainWithNoSourcesMisses covers the wiring that built a chain out of
// nothing: every lookup has to miss with ErrNotFound rather than return an
// empty value a caller would spend as a credential.
func TestChainWithNoSourcesMisses(t *testing.T) {
	t.Parallel()

	value, err := secrets.Chain().Get(t.Context(), testName)
	if !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("empty-chain error = %v, want ErrNotFound", err)
	}

	if value != "" {
		t.Errorf("empty-chain value = %q, want no value", value)
	}
}

// TestChainEnvSetToNothingStillShadows is the hazard below at its quietest. An
// operator who exported the variable empty reads it as unset, but env answers
// for the name, so the store is never asked and the empty string is served.
func TestChainEnvSetToNothingStillShadows(t *testing.T) {
	t.Parallel()

	rotatedStore := &fakeSource{values: map[string]string{testName: rotatedStoreValue}}

	value, err := secrets.Chain(envSource(""), rotatedStore).Get(t.Context(), testName)
	if err != nil {
		t.Fatalf("chain get: %v", err)
	}

	if value != "" {
		t.Fatalf("chain value = %q, want the empty env value", value)
	}

	if rotatedStore.calls != 0 {
		t.Errorf("store consulted %d times though an empty env var shadowed it, want 0", rotatedStore.calls)
	}
}

// TestChainEnvShadowsStore is the env-shadowing hazard: env stays
// first, so a stale env var silently shadows a rotated store value. A rotation
// can look done while the old secret is still served. The chain resolution here
// proves the hazard; the wiring surfaces it (startup warning) and the CLI marks
// it in ls, both in a later slice.
func TestChainEnvShadowsStore(t *testing.T) {
	t.Parallel()

	rotatedStore := &fakeSource{values: map[string]string{testName: rotatedStoreValue}}

	value, err := secrets.Chain(envSource(staleEnvValue), rotatedStore).Get(t.Context(), testName)
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
