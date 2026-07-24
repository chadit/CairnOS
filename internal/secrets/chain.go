package secrets

import (
	"context"
	"errors"
	"fmt"
)

// Source is the one-method shape every secret origin satisfies: the env source,
// the encrypted store, and the chain that composes them. It matches the engine's
// SecretSource, so a chain drops in wherever a single source did.
type Source interface {
	Get(ctx context.Context, name string) (string, error)
}

// chainSource tries each source in order. It is unexported; callers build one
// through Chain and hold it behind the Source interface.
type chainSource struct {
	sources []Source
}

// Chain composes sources into one Source that tries each in order. The wiring
// builds Chain(Env, store), which keeps env first: a value set in
// CAIRNOS_SECRET_<NAME> shadows the store for the same name. That precedence is
// an invariant, not an accident, and it is a hazard worth stating: a stale env
// var silently shadows a rotated store value, so a rotation can look done while
// the old secret is still served. The wiring surfaces the collision (a startup
// warning naming the shadowed store secrets) and the CLI marks it in ls; the
// resolution itself stays env-first here.
//
// Only ErrNotFound falls through to the next source. Any other error (a store
// outage, a bad key) returns immediately so a broken store never silently masks
// a value by falling back to env. That hard stop is deliberate: silent masking
// is the worse failure, so a store outage is an error, not a fallback.
func Chain(sources ...Source) Source {
	return chainSource{sources: sources}
}

// Get resolves name through the chain. It returns the first source's value, the
// first non-ErrNotFound error, or an ErrNotFound naming the secret and the
// number of sources tried when every source missed.
func (c chainSource) Get(ctx context.Context, name string) (string, error) {
	for _, source := range c.sources {
		value, err := source.Get(ctx, name)
		if err == nil {
			return value, nil
		}

		if !errors.Is(err, ErrNotFound) {
			return "", fmt.Errorf("secret %q: %w", name, err)
		}
	}

	return "", fmt.Errorf("%w: %s (tried %d sources)", ErrNotFound, name, len(c.sources))
}
