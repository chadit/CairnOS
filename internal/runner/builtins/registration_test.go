package builtins_test

import (
	"testing"

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

	for name, binary := range builtins.CLIDefaults() {
		if err := registry.Register(name, builtins.CLIFactory(name, binary)); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	factories := map[string]runner.Factory{
		builtins.EmailName:         builtins.EmailFactory,
		builtins.WebhookName:       builtins.WebhookFactory,
		builtins.CodeName:          builtins.CodeFactory,
		builtins.LanguageRulesName: builtins.LanguageRulesFactory,
		builtins.OpenAIName:        builtins.OpenAIFactory,
		builtins.AnthropicName:     builtins.AnthropicFactory,
		builtins.GitHubName:        builtins.GitHubFactory,
		builtins.MCPClientName:     builtins.MCPClientFactory,
		builtins.SandboxExecName:   builtins.SandboxExecFactory,
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
		builtins.GitHubName,
		builtins.LanguageRulesName,
		builtins.MCPClientName,
		builtins.OMPName,
		builtins.OMPCodexName,
		builtins.OMPQwenName,
		builtins.OpenAIName,
		builtins.QwenName,
		builtins.SandboxExecName,
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
