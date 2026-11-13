package plugin

import (
	"context"
	"fmt"

	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/pkg/runner"
)

// cfgKeyCommand is the required argv key in a [runner.<name>] plugin block.
const cfgKeyCommand = "command"

// cfgKeyEnvPassthrough names the optional host-env allowlist in a plugin block:
// the host variables to forward to the child on top of the minimal base env.
// Absent forwards nothing but PATH and the socket variable.
const cfgKeyEnvPassthrough = "env_passthrough"

// Factory adapts New to the registry contract for one configured name. The
// name comes from the config block header, so error codes and lookups always
// match what the operator wrote, whatever the plugin calls itself. ctx
// bounds construction (the registry builds runners synchronously at startup);
// the spawned process itself lives until Close.
func Factory(ctx context.Context, name string, lookup environ.Lookup) runner.Factory {
	return func(cfg map[string]any) (runner.Runner, error) {
		command, err := commandFromConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("configure plugin runner %q: %w", name, err)
		}

		passthrough, err := envPassthroughFromConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("configure plugin runner %q: %w", name, err)
		}

		plug, err := New(ctx, Config{Command: command, Name: name, EnvPassthrough: passthrough, Env: lookup})
		if err != nil {
			return nil, fmt.Errorf("start plugin runner %q: %w", name, err)
		}

		return plug, nil
	}
}

// envPassthroughFromConfig reads the optional env-passthrough allowlist, which
// TOML decodes as a []any of strings. An absent key forwards nothing.
func envPassthroughFromConfig(cfg map[string]any) ([]string, error) {
	raw, exists := cfg[cfgKeyEnvPassthrough]
	if !exists {
		return nil, nil
	}

	items, isList := raw.([]any)
	if !isList {
		return nil, fmt.Errorf("%w: env_passthrough must be an array of strings", ErrConfigValue)
	}

	names := make([]string, 0, len(items))

	for _, item := range items {
		text, isString := item.(string)
		if !isString {
			return nil, fmt.Errorf("%w: env_passthrough elements must be strings", ErrConfigValue)
		}

		names = append(names, text)
	}

	return names, nil
}

// commandFromConfig reads the required command argv, which TOML decodes as
// a []any of strings.
func commandFromConfig(cfg map[string]any) ([]string, error) {
	raw, exists := cfg[cfgKeyCommand]
	if !exists {
		return nil, ErrEmptyCommand
	}

	items, isList := raw.([]any)
	if !isList {
		return nil, fmt.Errorf("%w: command must be an array of strings", ErrConfigValue)
	}

	command := make([]string, 0, len(items))

	for _, item := range items {
		text, isString := item.(string)
		if !isString {
			return nil, fmt.Errorf("%w: command elements must be strings", ErrConfigValue)
		}

		command = append(command, text)
	}

	if len(command) == 0 {
		return nil, ErrEmptyCommand
	}

	return command, nil
}
