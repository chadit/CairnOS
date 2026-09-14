package secrets

import "strings"

// EnvVarName reports the environment variable the env source reads for a secret
// named name: the prefix plus the canonical name uppercased with dashes and
// dots folded to underscores, so it matches Get's own lookup. It returns a
// variable name, never a value, so callers can log or print the result.
func (e *Env) EnvVarName(name string) string {
	normalized := strings.ToUpper(name)
	normalized = strings.NewReplacer("-", "_", ".", "_").Replace(normalized)

	return e.prefix + normalized
}

// ShadowedByEnv returns the names whose env variable is set, so Chain(env,
// store) serves the env value, not the store row. lookup is injected
// (os.LookupEnv in production) so tests need no process environment.
//
// Env winning is deliberate, but a stale env var silently masks a rotated
// value, so callers surface the collision rather than resolve it here.
func ShadowedByEnv(names []string, lookup func(string) (string, bool)) []string {
	env := NewEnv()

	var shadowed []string

	for _, name := range names {
		if _, ok := lookup(env.EnvVarName(name)); ok {
			shadowed = append(shadowed, name)
		}
	}

	return shadowed
}
