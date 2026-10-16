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

// ChainSource tries each of its sources in order. Build one with Chain, which
// sets the order that decides which source shadows which.
type ChainSource struct {
	sources []Source
}

// Chain composes sources into one Source that tries each in order. The wiring
// builds Chain(Env, store), so CAIRNOS_SECRET_<NAME> shadows the store for the
// same name and a stale env var can make a rotation look done, which startup
// warns about.
//
// Only ErrNotFound falls through; any other error returns, so a broken store
// never masks a value with env.
func Chain(sources ...Source) *ChainSource {
	return &ChainSource{sources: sources}
}

// Get resolves name through the chain. It returns the first source's value or
// the first non-ErrNotFound error. When every source misses, it returns their
// misses joined, so each source's own hint survives: a chain of only Env still
// tells the operator which variable to set. The joined error matches
// ErrNotFound.
func (c *ChainSource) Get(ctx context.Context, name string) (string, error) {
	misses := make([]error, 0, len(c.sources))

	for _, source := range c.sources {
		value, err := source.Get(ctx, name)
		if err == nil {
			return value, nil
		}

		if !errors.Is(err, ErrNotFound) {
			return "", fmt.Errorf("secret %q: %w", name, err)
		}

		misses = append(misses, err)
	}

	// errors.Join of nothing is nil, which would read as a found empty value.
	if len(misses) == 0 {
		return "", fmt.Errorf("%w: %s (no secret sources configured)", ErrNotFound, name)
	}

	return "", errors.Join(misses...)
}
