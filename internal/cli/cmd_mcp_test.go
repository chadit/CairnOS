package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
)

// writeMCPConfig drops a config.toml carrying an [mcp] block into a fresh temp
// home and returns the directory, so the registry commands have settings to read
// without scaffolding a whole node.
func writeMCPConfig(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return dir
}

func TestMCPNoSubcommandIsUsageError(t *testing.T) {
	t.Parallel()

	code, _, stderr := runCLI(t, "mcp")
	if code != cli.ExitUsage {
		t.Fatalf("mcp with no subcommand exited %d, want %d", code, cli.ExitUsage)
	}

	if !strings.Contains(stderr, "serve") {
		t.Errorf("usage text should mention the serve subcommand; got %q", stderr)
	}
}

func TestMCPUnknownSubcommandIsUsageError(t *testing.T) {
	t.Parallel()

	code, _, stderr := runCLI(t, "mcp", "wat")
	if code != cli.ExitUsage {
		t.Fatalf("mcp wat exited %d, want %d", code, cli.ExitUsage)
	}

	if !strings.Contains(stderr, "wat") {
		t.Errorf("error should name the unknown subcommand; got %q", stderr)
	}
}

func TestMCPServeHelpExitsClean(t *testing.T) {
	t.Parallel()

	code, _, _ := runCLI(t, "mcp", "serve", "-h")
	if code != cli.ExitOK {
		t.Fatalf("mcp serve -h exited %d, want %d", code, cli.ExitOK)
	}
}

func TestMCPPublishWithoutAdvertiseURLIsUsageError(t *testing.T) {
	t.Parallel()

	dir := writeMCPConfig(t, "[mcp]\nregistry_url = \"https://registry.example/v0\"\n")

	code, _, stderr := runCLI(t, "mcp", "publish", "--dir", dir)
	if code != cli.ExitUsage {
		t.Fatalf("publish without advertise_url exited %d, want %d", code, cli.ExitUsage)
	}

	if !strings.Contains(stderr, "advertise_url") {
		t.Errorf("error should name the missing advertise_url; got %q", stderr)
	}
}

func TestMCPPublishDryRunPrintsRecord(t *testing.T) {
	t.Parallel()

	dir := writeMCPConfig(t, "[mcp]\nadvertise_url = \"https://node.example/mcp\"\n")

	code, stdout, stderr := runCLI(t, "mcp", "publish", "--dir", dir, "--dry-run")
	if code != cli.ExitOK {
		t.Fatalf("publish --dry-run exited %d: %s", code, stderr)
	}

	for _, want := range []string{"run_workflow", "get_language_rules", "https://node.example/mcp"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry-run record missing %q\n%s", want, stdout)
		}
	}
}

func TestMCPImportWithoutRegistryURLIsUsageError(t *testing.T) {
	t.Parallel()

	dir := writeMCPConfig(t, "[mcp]\nenabled = true\n")

	code, _, stderr := runCLI(t, "mcp", "import", "--dir", dir)
	if code != cli.ExitUsage {
		t.Fatalf("import without registry_url exited %d, want %d", code, cli.ExitUsage)
	}

	if !strings.Contains(stderr, "registry_url") {
		t.Errorf("error should name the missing registry_url; got %q", stderr)
	}
}
