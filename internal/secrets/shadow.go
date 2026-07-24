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

// ShadowedByEnv returns the subset of names whose env variable is set, so a
// Chain(env, store) would serve the env value and shadow the store row. lookup
// is injected (os.LookupEnv in production) so the check needs no process
// environment in tests. The result carries names only, never values, and is
// safe to surface in a startup warning or the secrets ls marker.
//
// The precedence is deliberate (env wins), but it is a hazard worth naming: a
// stale env var silently masks a rotated store value, so a rotation can look
// done while the old secret is still served. Callers surface the collision
// rather than resolve it here.
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
