package secrets

import (
	"strings"

	"github.com/chadit/CairnOS/internal/environ"
)

// EnvVarName reports the environment variable the env source reads for a secret
// named name: the prefix plus the canonical name uppercased with each dash and
// dot replaced by an underscore, so it matches Get's own lookup. It returns a
// variable name, never a value, so callers can log or print the result.
func (e *Env) EnvVarName(name string) string {
	normalized := strings.ToUpper(name)
	normalized = strings.NewReplacer("-", "_", ".", "_").Replace(normalized)

	return e.prefix + normalized
}

// ShadowedByEnv returns the names whose env variable is set, so Chain(env,
// store) serves the env value, not the store row. lookup is injected
// (environ.OS in production) so tests need no process environment.
//
// Env winning is deliberate, but a stale env var silently masks a rotated
// value, so callers surface the collision rather than resolve it here.
func ShadowedByEnv(names []string, lookup environ.Lookup) []string {
	env := NewEnvWith(lookup)

	var shadowed []string

	for _, name := range names {
		if _, ok := lookup(env.EnvVarName(name)); ok {
			shadowed = append(shadowed, name)
		}
	}

	return shadowed
}
