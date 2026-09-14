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
// builds Chain(Env, store), so CAIRNOS_SECRET_<NAME> shadows the store for the
// same name and a stale env var can make a rotation look done, which startup
// warns about.
//
// Only ErrNotFound falls through; any other error returns, so a broken store
// never masks a value with env.
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
