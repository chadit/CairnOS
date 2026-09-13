package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
)

// openaiCompatWithoutChatTOML turns the adapter on while chat stays disabled, the
// misconfiguration serve must reject at startup.
const openaiCompatWithoutChatTOML = `
# Telemetry off so the test never binds the default :9090 metrics port.
[telemetry]
enabled = false

[api]
grpc = "127.0.0.1:0"
http = "127.0.0.1:0"

[chat]
openai_compat = true
`

// TestServeFailsWhenOpenAICompatWithoutChat proves serve refuses to start when
// the OpenAI-compatible adapter is enabled but chat is off, since the adapter has
// no agent to drive (.agents/spec/parity/openai-compat.md, acceptance 8).
func TestServeFailsWhenOpenAICompatWithoutChat(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(openaiCompatWithoutChatTOML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "workflows"), 0o750); err != nil {
		t.Fatalf("create workflows dir: %v", err)
	}

	code, _, stderr := runCLI(t, serveCmd, flagDirArg, dir)

	if code != cli.ExitFailure {
		t.Fatalf("serve exit = %d, want failure\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stderr, "chat.openai_compat requires chat.enabled") {
		t.Errorf("stderr missing the config error; got: %s", stderr)
	}
}
