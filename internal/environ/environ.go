// Package environ passes a process environment around as a value. Code that
// reads os.Getenv directly can only be exercised by mutating the real
// environment, which forces those cases to run one at a time. Taking a Lookup
// instead lets a caller hand over a fixed set and stay parallel.
package environ

import (
	"maps"
	"os"
	"strings"
)

// Lookup answers one environment variable. The shape matches os.LookupEnv so
// the production wiring is os.LookupEnv itself and nothing adapts it.
type Lookup func(key string) (string, bool)

// OS reads the real process environment. This is the production default, and
// the only place outside a child process's own environment that touches it.
func OS() Lookup {
	return os.LookupEnv
}

// Map answers from a fixed set. The set is copied, so a caller that keeps
// mutating its map cannot change what an already-built Lookup answers.
func Map(vars map[string]string) Lookup {
	copied := maps.Clone(vars)

	return func(key string) (string, bool) {
		value, ok := copied[key]

		return value, ok
	}
}

// Slice answers from "K=V" pairs, the shape exec.Cmd.Env and os.Environ use.
// It is the inverse of Environ below, for code that already holds a child's
// environment as a slice and needs to read one variable out of it.
func Slice(pairs []string) Lookup {
	vars := make(map[string]string, len(pairs))

	for _, pair := range pairs {
		if key, value, found := strings.Cut(pair, "="); found {
			vars[key] = value
		}
	}

	return Map(vars)
}

// Get answers with the empty string when the variable is unset, the way
// os.Getenv does. A nil Lookup reads as an empty environment rather than
// panicking, so a zero-valued options struct still behaves.
func (l Lookup) Get(key string) string {
	if l == nil {
		return ""
	}

	value, _ := l(key)

	return value
}

// Environ renders the named variables as "K=V" pairs for exec.Cmd.Env, leaving
// out the ones that are unset. Naming the keys keeps a child process from
// inheriting whatever else happens to be in the parent's environment.
func Environ(lookup Lookup, keys ...string) []string {
	if lookup == nil {
		return nil
	}

	pairs := make([]string, 0, len(keys))

	for _, key := range keys {
		if value, ok := lookup(key); ok {
			pairs = append(pairs, key+"="+value)
		}
	}

	return pairs
}
