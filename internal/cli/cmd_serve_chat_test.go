package cli_test

import (
	"testing"

	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// builtUsageRegistry registers and builds the usage runner with no config
// block, the state serve reaches on a host that never wrote [runner.usage].
func builtUsageRegistry(t *testing.T) *runner.Registry {
	t.Helper()

	registry := runner.NewRegistry()

	if err := registry.Register(builtins.UsageName, builtins.UsageFactory(builtins.UsageOptions{})); err != nil {
		t.Fatalf("register usage: %v", err)
	}

	if err := registry.BuildAll(nil); err != nil {
		t.Fatalf("build registry: %v", err)
	}

	return registry
}

func TestChatReadToolsReusesTheRegistryUsageRunner(t *testing.T) {
	t.Parallel()

	registry := builtUsageRegistry(t)

	built, _ := registry.Get(builtins.UsageName)

	got, present := cli.ServeChatReadTools(registry)[chat.ToolUsage]
	if !present {
		t.Fatalf("chat read tools lack %s although the registry built it", chat.ToolUsage)
	}

	if got != built {
		t.Fatalf("chat usage tool is a second instance, want the registry's own")
	}
}

func TestChatReadToolsLeaveUsageOutWithoutARegistryInstance(t *testing.T) {
	t.Parallel()

	cases := []struct {
		registry *runner.Registry
		name     string
	}{
		{name: "nil registry", registry: nil},
		{name: "registry without usage", registry: runner.NewRegistry()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, present := cli.ServeChatReadTools(tc.registry)[chat.ToolUsage]; present {
				t.Fatalf("chat read tools carry %s with no built runner", chat.ToolUsage)
			}
		})
	}
}
