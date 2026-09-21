package plugin_test

import (
	"os"
	"testing"

	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/runner/plugin"
)

// Host env names the L2 least-privilege tests drive. The secret stands in for a
// real token or signing key the host process holds.
const (
	hostSecretName  = "CAIRNOS_TEST_HOST_SECRET"
	hostSecretValue = "super-secret-token"
)

// hostEnv is the parent environment the plugin is spawned under. PATH is the
// real one so the child can still find an interpreter; the secret is staged
// here rather than in the process so the case stays parallel.
func hostEnv() environ.Lookup {
	return environ.Map(map[string]string{
		"PATH":         os.Getenv("PATH"),
		hostSecretName: hostSecretValue,
	})
}

// echoEnvValue spawns one echo plugin under the given passthrough allowlist,
// asks it to echo the host secret variable, and returns the value the child
// saw. An empty string means the child's environment did not carry the secret.
func echoEnvValue(t *testing.T, passthrough []string) string {
	t.Helper()

	plug, err := plugin.New(t.Context(), plugin.Config{
		Command:        []string{buildEchoPlugin(t)},
		Name:           echoName,
		EnvPassthrough: passthrough,
		Env:            hostEnv(),
	})
	if err != nil {
		t.Fatalf("new echo plugin: %v", err)
	}

	t.Cleanup(func() { _ = plug.Close() })

	result, err := plug.Execute(t.Context(), echoTask(map[string]any{"echo_env": hostSecretName}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	output, isMap := result.Output.(map[string]any)
	if !isMap {
		t.Fatalf("output is not a map: %#v", result.Output)
	}

	value, _ := output["env_value"].(string)

	return value
}

// TestPluginDoesNotInheritHostSecret proves the L2 least-privilege default: a
// host secret variable does not reach the plugin child unless it is
// allowlisted, and does reach it when env_passthrough names it.
func TestPluginDoesNotInheritHostSecret(t *testing.T) {
	t.Parallel()

	if got := echoEnvValue(t, nil); got != "" {
		t.Fatalf("plugin child saw host secret %q with no allowlist; the base env must exclude it", got)
	}

	if got := echoEnvValue(t, []string{hostSecretName}); got != hostSecretValue {
		t.Fatalf("allowlisted host var reached child as %q, want %q", got, hostSecretValue)
	}
}
