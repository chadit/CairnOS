// Package secrets resolves secret references from a configured source.
// Phase 0 ships the environment-variable source; file, Vault, and cloud
// sources implement the same shape later and are chosen by config
// (.agents/spec/11-deployment-and-config.md, "Secrets").
package secrets

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// EnvPrefix is the environment namespace for secret values: the workflow
// reference secrets.db_password reads CAIRNOS_SECRET_DB_PASSWORD.
const EnvPrefix = "CAIRNOS_SECRET_"

// Env reads secrets from prefixed environment variables. It satisfies the
// engine's SecretSource interface structurally.
type Env struct {
	prefix string
}

// NewEnv builds the environment source with the standard prefix.
func NewEnv() *Env {
	return &Env{prefix: EnvPrefix}
}

// Get resolves one secret name. Names are uppercased and dashes/dots become
// underscores, so secrets.db-password and secrets.db_password read the same
// variable rather than failing on a styling difference.
func (e *Env) Get(ctx context.Context, name string) (string, error) {
	// Environment lookups cannot block, but honoring an already-canceled
	// context keeps this source interchangeable with remote ones that can.
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("secret lookup: %w", err)
	}

	normalized := strings.ToUpper(name)
	normalized = strings.NewReplacer("-", "_", ".", "_").Replace(normalized)

	value, found := os.LookupEnv(e.prefix + normalized)
	if !found {
		return "", fmt.Errorf("%w: %s (set %s%s)", ErrNotFound, name, e.prefix, normalized)
	}

	return value, nil
}
