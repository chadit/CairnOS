package cli_test

import (
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
)

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
