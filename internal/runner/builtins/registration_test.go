package builtins_test

import (
	"slices"
	"testing"

	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// TestPhase1aFactoriesBuildWithEmptyConfig mirrors the CLI wiring: every
// Phase 1a factory must register and build with no config block at all,
// because the registry constructs all built-ins at startup whether or not
// the operator configured them.
func TestPhase1aFactoriesBuildWithEmptyConfig(t *testing.T) {
	t.Parallel()

	registry := runner.NewRegistry()

	for name, tool := range builtins.CLIDefaults() {
		if err := registry.Register(name, builtins.CLIFactory(name, tool, nil, nil)); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	factories := map[string]runner.Factory{
		builtins.EmailName:         builtins.EmailFactory(environ.Map(nil)),
		builtins.WebhookName:       builtins.WebhookFactory(),
		builtins.CodeName:          builtins.CodeFactory(),
		builtins.LanguageRulesName: builtins.LanguageRulesFactory(),
		builtins.OpenAIName:        builtins.OpenAIFactory(environ.Map(nil)),
		builtins.AnthropicName:     builtins.AnthropicFactory(environ.Map(nil)),
		builtins.GitHubName:        builtins.GitHubFactory(environ.Map(nil)),
		builtins.MCPClientName:     builtins.MCPClientFactory(),
		builtins.SandboxExecName:   builtins.SandboxExecFactory(),
		builtins.ExecName:          builtins.ExecFactory(),
		builtins.UsageName:         builtins.UsageFactory(builtins.UsageOptions{}),
	}

	for name, factory := range factories {
		if err := registry.Register(name, factory); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	if err := registry.BuildAll(nil); err != nil {
		t.Fatalf("build all with empty config: %v", err)
	}

	wantNames := []string{
		builtins.AnthropicName,
		builtins.AntigravityName,
		builtins.CodeName,
		builtins.CodexName,
		builtins.EmailName,
		builtins.ExecName,
		builtins.GitHubName,
		builtins.LanguageRulesName,
		builtins.MCPClientName,
		builtins.OMPName,
		builtins.OpenAIName,
		builtins.QwenName,
		builtins.SandboxExecName,
		builtins.UsageName,
		builtins.WebhookName,
	}

	for _, name := range wantNames {
		instance, exists := registry.Get(name)
		if !exists {
			t.Fatalf("runner %s not built", name)
		}

		if instance.Name() != name {
			t.Fatalf("runner %s reports name %s", name, instance.Name())
		}
	}
}

// TestExecBuildsFromItsConfigBlock covers the other half of the registration
// contract: a [runner.exec] block reaches the factory through BuildAll and the
// built instance shows up under its own name.
func TestExecBuildsFromItsConfigBlock(t *testing.T) {
	t.Parallel()

	registry := runner.NewRegistry()

	if err := registry.Register(builtins.ExecName, builtins.ExecFactory()); err != nil {
		t.Fatalf("register %s: %v", builtins.ExecName, err)
	}

	cfgs := map[string]map[string]any{
		builtins.ExecName: {
			cfgAllow:   []any{actionReconcile},
			cfgTimeout: int64(540),
		},
	}

	if err := registry.BuildAll(cfgs); err != nil {
		t.Fatalf("build all with an exec block: %v", err)
	}

	if names := registry.List(); !slices.Contains(names, builtins.ExecName) {
		t.Fatalf("list = %v, want it to contain %q", names, builtins.ExecName)
	}

	instance, exists := registry.Get(builtins.ExecName)
	if !exists {
		t.Fatalf("runner %s not built", builtins.ExecName)
	}

	task := execTask(map[string]any{cmdKey: binTrue})
	if err := instance.Validate(task); err != nil {
		t.Fatalf("configured runner refused its allowed action: %v", err)
	}
}
