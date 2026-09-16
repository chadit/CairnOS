package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/bitwarden"
	"github.com/chadit/CairnOS/internal/cli"
)

// The answers a piped stdin gives the four prompts, and the chezmoi fixture's
// answers to the same four.
const (
	authTypedItem   = "typed-item"
	authTypedID     = "typed-id"
	authTypedSecret = "typed-secret"
	authTypedKey    = "typed-key"

	chezmoiItem   = "chezmoi-item"
	chezmoiID     = "chezmoi-id"
	chezmoiSecret = "chezmoi-secret"
	chezmoiKey    = "chezmoi-key"
)

// authSubcmd is the subcommand under test.
const authSubcmd = "auth"

// credentialsFileName is where `secrets auth` writes with no config block, which
// is the state a fresh host is in.
const credentialsFileName = "bitwarden.env"

// runAuth drives the command over a piped stdin, which is how every prompt case
// answers without a terminal.
func runAuth(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	argv := append([]string{secretsCmd, authSubcmd}, args...)
	code := pinnedApp(t, &stdout, &stderr, testBinaryDigest(t)).
		WithStdin(strings.NewReader(stdin)).
		Run(t.Context(), argv)

	return code, stdout.String(), stderr.String()
}

// emptyHome points the chezmoi lookup at a home with no chezmoi config, so a
// case about typed answers is not quietly answered by the developer's own
// workstation config.
func emptyHome(t *testing.T) {
	t.Helper()

	t.Setenv("HOME", t.TempDir())
}

// writeChezmoiConfig stages the config `secrets auth` reads its defaults from.
func writeChezmoiConfig(t *testing.T) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".config", "chezmoi")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("stage the chezmoi config directory: %v", err)
	}

	body := "[data.secret]\n" +
		"bw_item_name = \"" + chezmoiItem + "\"\n" +
		"bw_client_id = \"" + chezmoiID + "\"\n" +
		"bw_client_secret = \"" + chezmoiSecret + "\"\n" +
		"bw_master_key = \"" + chezmoiKey + "\"\n"

	writePullFile(t, filepath.Join(dir, "chezmoi.toml"), body)
}

// readCredentials returns the written file, failing the test when it is missing.
func readCredentials(t *testing.T, dir string) string {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(dir, credentialsFileName)) // #nosec G304 -- the path is built by the test
	if err != nil {
		t.Fatalf("read the credentials file: %v", err)
	}

	return string(body)
}

// TestSecretsAuthWritesWhatWasTyped covers FR14: a host with no credentials file
// gets one from four answers, at the mode the loader insists on.
//
//nolint:paralleltest // t.Setenv rewrites HOME for the whole process
func TestSecretsAuthWritesWhatWasTyped(t *testing.T) {
	emptyHome(t)

	dir := t.TempDir()
	typed := strings.Join([]string{authTypedItem, authTypedID, authTypedSecret, authTypedKey}, "\n") + "\n"

	code, stdout, stderr := runAuth(t, typed, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	written := readCredentials(t, dir)

	for _, want := range []string{
		assignment(bitwarden.EnvItem, authTypedItem),
		assignment(bitwarden.EnvClientID, authTypedID),
		assignment(bitwarden.EnvClientSecret, authTypedSecret),
		assignment(bitwarden.EnvMasterKey, authTypedKey),
	} {
		if !strings.Contains(written, want) {
			t.Fatalf("credentials file missing %q:\n%s", want, written)
		}
	}

	// The item name is operator-visible; the three credentials are not, so a
	// terminal transcript or a scrollback buffer must not carry them.
	for _, secret := range []string{authTypedID, authTypedSecret, authTypedKey} {
		if strings.Contains(stdout, secret) {
			t.Fatalf("stdout echoed a credential back:\n%s", stdout)
		}
	}

	// A redirected stdin shows nobody anything, so the warning about visible
	// typing would be noise on every scripted run.
	if strings.Contains(stdout, "visible") {
		t.Fatalf("a redirected stdin warned about visible typing:\n%s", stdout)
	}

	info, err := os.Stat(filepath.Join(dir, credentialsFileName))
	if err != nil {
		t.Fatalf("stat the credentials file: %v", err)
	}

	if info.Mode().Perm() != pullPerm {
		t.Fatalf("credentials file is mode %04o, want %04o", info.Mode().Perm(), pullPerm)
	}
}

// TestSecretsAuthTakesChezmoiDefaults covers FR15: the operator already holds
// these four values on the workstation, so four Enters reuse them rather than
// asking for a second copy.
//
//nolint:paralleltest // t.Setenv rewrites HOME for the whole process
func TestSecretsAuthTakesChezmoiDefaults(t *testing.T) {
	writeChezmoiConfig(t)

	dir := t.TempDir()

	code, stdout, stderr := runAuth(t, "\n\n\n\n", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	// The item name is shown so the operator can see what will be used; the
	// three secrets are offered as "from chezmoi" and never printed, which is
	// the whole reason defaultHint treats them apart.
	for _, secret := range []string{chezmoiID, chezmoiSecret, chezmoiKey} {
		if strings.Contains(stdout, secret) {
			t.Fatalf("a chezmoi secret was printed at the prompt:\n%s", stdout)
		}
	}

	written := readCredentials(t, dir)

	for _, want := range []string{
		assignment(bitwarden.EnvItem, chezmoiItem),
		assignment(bitwarden.EnvClientID, chezmoiID),
		assignment(bitwarden.EnvClientSecret, chezmoiSecret),
		assignment(bitwarden.EnvMasterKey, chezmoiKey),
	} {
		if !strings.Contains(written, want) {
			t.Fatalf("credentials file missing %q:\n%s", want, written)
		}
	}
}

// TestSecretsAuthFailsOnAClosedStdin covers FR17. A deploy script that reaches
// this command has no terminal behind it, so the run has to end rather than wait
// for a line that cannot arrive.
//
//nolint:paralleltest // t.Setenv rewrites HOME for the whole process
func TestSecretsAuthFailsOnAClosedStdin(t *testing.T) {
	emptyHome(t)

	dir := t.TempDir()

	code, _, stderr := runAuth(t, "", flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want failure", code)
	}

	if !strings.Contains(stderr, bitwarden.EnvClientID) {
		t.Fatalf("stderr does not name the variable that went unanswered:\n%s", stderr)
	}

	if _, err := os.Stat(filepath.Join(dir, credentialsFileName)); err == nil {
		t.Fatal("a failed prompt still wrote the credentials file")
	}
}

// TestSecretsAuthChecksAnExistingFile covers FR16: the credentials already on
// the host are proven against the vault here, rather than at the next deploy.
func TestSecretsAuthChecksAnExistingFile(t *testing.T) {
	binDir := stageFakeBW(t, map[string]string{
		cairnosOneVar: "one",
		"CAIRNOS_TWO": "two",
		pullFieldOne:  pullValueOne,
	})

	dir := t.TempDir()
	writePullFile(t, filepath.Join(dir, credentialsFileName), credentialsFixtureFor(pullItem))

	t.Setenv("PATH", pathWith(binDir))

	code, stdout, stderr := runAuth(t, "", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	for _, want := range []string{pullItem, "2", "locked"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("status output missing %q:\n%s", want, stdout)
		}
	}
}

// TestSecretsAuthReportsAMissingItem is the other half of the status check: the
// credentials open the vault, and the item they name is not in it.
func TestSecretsAuthReportsAMissingItem(t *testing.T) {
	binDir := stageFakeBW(t, map[string]string{cairnosOneVar: "one"})

	dir := t.TempDir()
	writePullFile(t, filepath.Join(dir, credentialsFileName), credentialsFixtureFor("absent-item"))

	t.Setenv("PATH", pathWith(binDir))

	code, _, stderr := runAuth(t, "", flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want failure", code)
	}

	if !strings.Contains(stderr, "absent-item") {
		t.Fatalf("stderr does not name the item it could not find:\n%s", stderr)
	}
}

// credentialsFixtureFor is credentialsFixture with the item name the status
// check reads, built from the exported names so no credential-shaped literal
// appears in this file.
func credentialsFixtureFor(item string) string {
	return bitwarden.EnvItem + "=" + item + "\n" + credentialsFixture()
}

// assignment renders the line the credentials file carries for one variable.
// The value is wrapped because the loader strips one surrounding pair, which is
// what lets a secret hold quotes of its own.
func assignment(name, value string) string {
	return name + "='" + value + "'"
}

// TestSecretsAuthRoundTripsASecretHoldingQuotes is the reason for the wrap. The
// loader strips one matching pair and processes no escapes, so a master password
// that begins and ends with a quote comes back two characters shorter without
// it, and the vault refuses an unlock that names nothing about the file.
//
//nolint:paralleltest // t.Setenv rewrites HOME for the whole process
func TestSecretsAuthRoundTripsASecretHoldingQuotes(t *testing.T) {
	emptyHome(t)

	const awkward = "'it's \"quoted\"'"

	dir := t.TempDir()
	typed := strings.Join([]string{authTypedItem, authTypedID, authTypedSecret, awkward}, "\n") + "\n"

	if code, _, stderr := runAuth(t, typed, flagDirArg, dir); code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	creds, err := bitwarden.LoadCredentials(filepath.Join(dir, credentialsFileName), func(string) string { return "" })
	if err != nil {
		t.Fatalf("load the credentials back: %v", err)
	}

	if creds.MasterKey != awkward {
		t.Fatalf("master key round-tripped as %q, want %q", creds.MasterKey, awkward)
	}
}
