package config

import (
	"fmt"
	"maps"
	"slices"
)

// runnerKeyEnv names the API-key variable a `[runner.<name>]` block reads. The
// block name decides it, so two blocks of the same type pointed at different
// vendors each carry their own key rather than sharing one.
//
// The name goes through EnvToken, the same lossy mapping host and CLI names
// use, which is why validateRunnerNames refuses two blocks that land on one
// variable.
func runnerKeyEnv(block string) string {
	return envPrefix + "RUNNER_" + EnvToken(block) + "_API_KEY"
}

// RunnerKeyEnvs lists the variables a block reads, most specific first: its own
// name, then the variable named after its type. The fallback is what keeps a
// host that only ever set CAIRNOS_RUNNER_OPENAI_API_KEY working after a block
// gains a name of its own.
//
// A block named after its type collapses to one name, because both rules
// produce the same string and an error naming it twice reads like a bug.
func RunnerKeyEnvs(block, typeEnv string) []string {
	named := runnerKeyEnv(block)
	if named == typeEnv {
		return []string{typeEnv}
	}

	return []string{named, typeEnv}
}

// validateRunnerNames refuses two blocks whose names render as one API-key
// variable. EnvToken is lossy, so "z-ai" and "z_ai" are two config names and
// one key; picking a winner would hand one vendor the other's credential, and
// nothing at run time would say so.
func validateRunnerNames(runners map[string]map[string]any) error {
	seen := make(map[string]string, len(runners))

	for _, name := range slices.Sorted(maps.Keys(runners)) {
		token := EnvToken(name)

		if other, taken := seen[token]; taken {
			return fmt.Errorf("%w: runner.%s and runner.%s both render as %s; rename one",
				ErrInvalidConfig, other, name, runnerKeyEnv(name))
		}

		seen[token] = name
	}

	return nil
}
