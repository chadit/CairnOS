package cli_test

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
)

const cmdServiceName = "service"

func TestServiceWithoutSubcommandPrintsUsage(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer

	code := cli.New(&stdout, &stderr).Run(t.Context(), []string{cmdServiceName})
	if code != cli.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, cli.ExitUsage)
	}

	if !strings.Contains(stderr.String(), "setup") || !strings.Contains(stderr.String(), "uninstall") {
		t.Errorf("usage missing subcommands:\n%s", stderr.String())
	}
}

func TestServiceUnknownSubcommandIsUsageError(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer

	code := cli.New(&stdout, &stderr).Run(t.Context(), []string{cmdServiceName, "restart"})
	if code != cli.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, cli.ExitUsage)
	}

	if !strings.Contains(stderr.String(), `unknown service subcommand "restart"`) {
		t.Errorf("stderr = %s", stderr.String())
	}
}

func TestServiceSetupDryRunPrintsPlanAndWritesNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	var stdout, stderr bytes.Buffer

	code := cli.New(&stdout, &stderr).Run(t.Context(),
		[]string{cmdServiceName, "setup", "--dry-run", flagDirArg, dir})

	// Hosts without a supported supervisor (or a root test runner) refuse
	// with a mode or support error; the dry-run contract only applies where
	// setup itself would run.
	if code != cli.ExitOK {
		t.Skipf("host does not support a user service install here: %s", stderr.String())
	}

	if !strings.Contains(stdout.String(), "plan:") {
		t.Errorf("dry-run must print the plan:\n%s", stdout.String())
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("dry-run wrote into the home dir: %v", entries)
	}
}
