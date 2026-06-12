package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
)

// Command names and flags the vault and rules CLI tests reference, named so
// the assertions read as the command surface rather than scattered literals.
const (
	vaultCmd = "vault"
	rulesCmd = "rules"
	langGo   = "go"
)

func TestVaultPutGetQueryLifecycle(t *testing.T) {
	t.Parallel()

	dir := initDir(t)

	code, stdout, stderr := runCLI(t, vaultCmd, "put",
		"--title", "design", "--body", "kafka lag note", "--tags", "infra", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("vault put exited %d: %s", code, stderr)
	}

	id := strings.TrimSpace(stdout)
	if id == "" {
		t.Fatal("vault put printed no id")
	}

	code, stdout, _ = runCLI(t, vaultCmd, "get", id, flagDirArg, dir)
	if code != cli.ExitOK || !strings.Contains(stdout, "kafka lag note") {
		t.Fatalf("vault get (exit %d) missing body:\n%s", code, stdout)
	}

	code, stdout, _ = runCLI(t, vaultCmd, "query", "--text", "kafka", flagDirArg, dir)
	if code != cli.ExitOK || !strings.Contains(stdout, id) {
		t.Fatalf("vault query (exit %d) missing the document:\n%s", code, stdout)
	}
}

func TestRulesSetGetMerge(t *testing.T) {
	t.Parallel()

	dir := initDir(t)

	// A language default and a project override on the same key.
	if code, _, stderr := runCLI(t, rulesCmd, "set", langGo, "max_line", "120", flagDirArg, dir); code != cli.ExitOK {
		t.Fatalf("rules set default exited %d: %s", code, stderr)
	}

	if code, _, stderr := runCLI(t, rulesCmd, "set", langGo, "finance-app", "max_line", "100", flagDirArg, dir); code != cli.ExitOK {
		t.Fatalf("rules set override exited %d: %s", code, stderr)
	}

	code, stdout, _ := runCLI(t, rulesCmd, "get", langGo, "finance-app", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("rules get exited %d", code)
	}

	// The project override wins, so the merged value is 100, not 120.
	if !strings.Contains(stdout, "100") || strings.Contains(stdout, "120") {
		t.Fatalf("rules get merged output = %q, want the 100 override not the 120 default", stdout)
	}
}

func TestRulesImportSeedsFromStaticFiles(t *testing.T) {
	t.Parallel()

	dir := initDir(t)

	// A static rules dir in the old <lang>[.<project>].toml stub format.
	srcDir := filepath.Join(t.TempDir(), "rules")
	if err := os.MkdirAll(srcDir, 0o750); err != nil {
		t.Fatalf("create rules src dir: %v", err)
	}

	writeRuleFile(t, srcDir, "go.toml", "formatting = \"gofmt\"\nerrors = \"wrap\"\n")
	writeRuleFile(t, srcDir, "go.finance-app.toml", "errors = \"apperr\"\n")

	code, stdout, stderr := runCLI(t, rulesCmd, "import", srcDir, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("rules import exited %d: %s", code, stderr)
	}

	if !strings.Contains(stdout, "imported") {
		t.Fatalf("rules import output = %q, want a count line", stdout)
	}

	// The imported override must win the merge: errors=apperr, not the wrap default.
	code, stdout, _ = runCLI(t, rulesCmd, "get", langGo, "finance-app", flagDirArg, dir)
	if code != cli.ExitOK || !strings.Contains(stdout, "apperr") {
		t.Fatalf("rules get after import (exit %d) missing the imported override:\n%s", code, stdout)
	}
}

// writeRuleFile drops a static rules file into srcDir.
func writeRuleFile(t *testing.T, srcDir, name, content string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(srcDir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write rules file %s: %v", name, err)
	}
}
