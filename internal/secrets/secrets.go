// Package secrets resolves secret references from a configured source.
// Phase 0 ships the environment-variable source; file, Vault, and cloud
// sources implement the same shape later and are chosen by config
// (.agents/spec/11-deployment-and-config.md, "Secrets").
//
// The encrypted store adds AES-256-GCM at-rest sealing (Sealer) under a 32-byte
// master key loaded from the environment (LoadKeys). A row binds its workspace,
// name, and a recency stamp as authenticated data, so a dumped row cannot be
// re-pointed at another secret or rolled back to an older value without the open
// failing.
//
// Threat model (three boundaries, stated so a reader does not over-trust the
// word "encrypted"):
//   - The store defends a database-dump / read-at-rest attacker only, never a
//     process that can inject environment variables. Env stays trusted in the
//     single-operator model, and Chain(env, store) puts env first, so an env
//     value always wins.
//   - A secret interpolated into an action's output is recoverable in plaintext
//     from run history. This package encrypts the secret store, not the run
//     store; run-output redaction is a separate concern.
//   - Env-sourced secrets stay visible to child CLI runners through the
//     inherited process environment.
//
// The master key must come from a CSPRNG (GenerateKey / cairnos secrets keygen).
// LoadKeys checks length, never entropy, so a low-entropy 32-byte key passes yet
// is insecure. Key compromise loses the whole store; the previous-key rotation
// window is the only partial mitigation.
package secrets

import (
	"context"
	"fmt"
	"strings"

	"github.com/chadit/CairnOS/internal/environ"
)

// EnvPrefix is the environment namespace for secret values: the workflow
// reference secrets.db_password reads CAIRNOS_SECRET_DB_PASSWORD.
const EnvPrefix = "CAIRNOS_SECRET_"

// Env reads secrets from prefixed environment variables. It satisfies the
// engine's SecretSource interface structurally.
type Env struct {
	lookup environ.Lookup
	prefix string
}

// NewEnv builds the environment source over this process's own environment.
func NewEnv() *Env {
	return NewEnvWith(environ.OS())
}

// NewEnvWith builds the environment source over a supplied one. Taking the
// lookup lets a caller hand this source a fixed set instead of exporting a
// credential every other part of the process could then read.
func NewEnvWith(lookup environ.Lookup) *Env {
	return &Env{prefix: EnvPrefix, lookup: lookup}
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

	value, found := e.lookup(e.prefix + normalized)
	if !found {
		return "", fmt.Errorf("%w: %s (set %s%s)", ErrNotFound, name, e.prefix, normalized)
	}

	return value, nil
}
