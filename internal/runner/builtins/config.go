package builtins

import (
	"fmt"

	"github.com/chadit/CairnOS/pkg/runner"
)

// wrapConfigErr tags a config decode failure with the runner it belongs to,
// so every factory reports the same "configure <name>" shape.
func wrapConfigErr(name string, err error) error {
	return fmt.Errorf("configure %s: %w", name, err)
}

// asFactory adapts a constructor for one concrete runner to the registry's
// Factory: Go function types are not covariant, so a func returning *Exec is
// not a runner.Factory even though *Exec is a runner.Runner. The error path
// returns an untyped nil so a nil *T never reaches the registry as a non-nil
// Runner.
func asFactory[T runner.Runner](build func(map[string]any) (T, error)) runner.Factory {
	return func(cfg map[string]any) (runner.Runner, error) {
		built, err := build(cfg)
		if err != nil {
			return nil, err
		}

		return built, nil
	}
}

// configValue reads one typed key from a runner's config block, falling back
// when the key is absent. A present key with the wrong type is a build error:
// silently ignoring it would make config typos invisible.
func configValue[T any](cfg map[string]any, key string, fallback T) (T, error) {
	raw, exists := cfg[key]
	if !exists {
		return fallback, nil
	}

	typed, matches := raw.(T)
	if !matches {
		var zero T

		return zero, fmt.Errorf("%w: %s has type %T", ErrConfigValue, key, raw)
	}

	return typed, nil
}

// configStrings reads a string-list key. TOML arrays decode as []any, so the
// elements convert one by one. A nil result means the key is absent.
func configStrings(cfg map[string]any, key string) ([]string, error) {
	raw, exists := cfg[key]
	if !exists {
		return nil, nil
	}

	items, isList := raw.([]any)
	if !isList {
		return nil, fmt.Errorf("%w: %s must be an array of strings", ErrConfigValue, key)
	}

	texts := make([]string, 0, len(items))

	for _, item := range items {
		text, isString := item.(string)
		if !isString {
			return nil, fmt.Errorf("%w: %s elements must be strings", ErrConfigValue, key)
		}

		texts = append(texts, text)
	}

	return texts, nil
}

// configStringMap reads a string-to-string config table. TOML decodes a table
// as map[string]any, so the values convert one by one. An absent key yields an
// empty map, which merges as a no-op.
func configStringMap(cfg map[string]any, key string) (map[string]string, error) {
	raw, exists := cfg[key]
	if !exists {
		return map[string]string{}, nil
	}

	table, isTable := raw.(map[string]any)
	if !isTable {
		return nil, fmt.Errorf("%w: %s must be a table of strings", ErrConfigValue, key)
	}

	out := make(map[string]string, len(table))

	for name, value := range table {
		text, isString := value.(string)
		if !isString {
			return nil, fmt.Errorf("%w: %s values must be strings", ErrConfigValue, key)
		}

		out[name] = text
	}

	return out, nil
}

// requireStringParam checks one mandatory string param. Unresolved references
// still read as strings, so load-time validation accepts them and the
// launch-time pass sees the final value, matching the two-phase contract in
// pkg/runner.
func requireStringParam(task *runner.Task, key, codePrefix string) error {
	raw, exists := task.Params[key]
	if !exists {
		return taskError(runner.ClassPermanent, codePrefix+".missing_"+key, "params must include %s", key)
	}

	if _, isString := raw.(string); !isString {
		return paramError(codePrefix, key, "must be a string")
	}

	return nil
}

// optionalStringParam checks one optional param, which must be a string when
// present.
func optionalStringParam(task *runner.Task, key, codePrefix string) error {
	raw, exists := task.Params[key]
	if !exists {
		return nil
	}

	if _, isString := raw.(string); !isString {
		return paramError(codePrefix, key, "must be a string")
	}

	return nil
}
