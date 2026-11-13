package cli_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/bitwarden"
	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/config"
	"github.com/chadit/CairnOS/internal/environ"
)

// The answers a piped stdin gives the four prompts, and the chezmoi fixture's
// three credentials.
const (
	authTypedItem   = "typed-item"
	authTypedID     = "typed-id"
	authTypedSecret = "typed-secret"
	authTypedKey    = "typed-key"

	chezmoiID     = "chezmoi-id"
	chezmoiSecret = "chezmoi-secret"
	chezmoiKey    = "chezmoi-key"
)

// authSubcmd is the subcommand under test.
const authSubcmd = "auth"

// credentialsFileName is where `secrets auth` writes with no config block, which
// is the state a fresh host is in.
const credentialsFileName = "bitwarden.env"

// chezmoiPointer is the credentials_file `secrets auth` writes for a chezmoi
// config at its default place under the home.
const chezmoiPointer = "~/.config/chezmoi/chezmoi.toml"

// runAuth drives the command over a piped stdin and a staged environment, which
// is how every prompt case answers without a terminal and without moving the
// process HOME out from under the rest of the suite.
func runAuth(t *testing.T, env map[string]string, stdin string, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	argv := append([]string{secretsCmd, authSubcmd}, args...)
	code := pinnedApp(t, &stdout, &stderr, testBinaryDigest(t)).
		WithStdin(strings.NewReader(stdin)).
		WithEnv(environ.Map(env)).
		Run(t.Context(), argv)

	return code, stdout.String(), stderr.String()
}

// typedAnswers is what a piped stdin carries for the four prompts, one line
// each in the order the command asks them. Only the item and the master
// password vary between cases; the API key is always the typed fixture.
func typedAnswers(item, masterKey string) string {
	return strings.Join([]string{item, authTypedID, authTypedSecret, masterKey}, "\n") + "\n"
}

// emptyHome points the chezmoi lookup at a home with no chezmoi config, so a
// case about typed answers is not quietly answered by the developer's own
// workstation config.
func emptyHome(t *testing.T) map[string]string {
	t.Helper()

	return map[string]string{cli.HomeEnvName(): t.TempDir()}
}

// chezmoiBody is a chezmoi config carrying the item and all three credentials.
func chezmoiBody() string {
	return "[data.secret]\n" +
		"bw_item_name = \"" + pullItem + "\"\n" +
		chezmoiKeys()
}

// chezmoiKeys is the three credentials a chezmoi config holds, without the item.
func chezmoiKeys() string {
	return "bw_client_id = \"" + chezmoiID + "\"\n" +
		"bw_client_secret = \"" + chezmoiSecret + "\"\n" +
		"bw_master_key = \"" + chezmoiKey + "\"\n"
}

// stageChezmoiConfig writes body as chezmoi's config under configHome, the
// directory chezmoi reads $XDG_CONFIG_HOME or ~/.config as, and returns its path.
func stageChezmoiConfig(t *testing.T, configHome, body string) string {
	t.Helper()

	dir := filepath.Join(configHome, "chezmoi")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("stage the chezmoi config directory: %v", err)
	}

	path := filepath.Join(dir, "chezmoi.toml")
	writePullFile(t, path, body)

	return path
}

// chezmoiHome stages a home whose ~/.config holds body as chezmoi's config and
// returns the environment pointing at that home.
func chezmoiHome(t *testing.T, body string) map[string]string {
	t.Helper()

	home := t.TempDir()
	stageChezmoiConfig(t, filepath.Join(home, ".config"), body)

	return map[string]string{cli.HomeEnvName(): home}
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

// assertAbsent fails the test when path exists, naming what should not have
// been written.
func assertAbsent(t *testing.T, path, what string) {
	t.Helper()

	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s exists at %s (stat error %v), want none", what, path, err)
	}
}

// TestSecretsAuthWritesWhatWasTyped proves a host with no credentials file
// gets one from four answers, at the mode the loader insists on. The three
// credentials are not operator-visible, so no transcript may carry them, and a
// redirected stdin skips the visible-typing warning. The next step it names
// carries the --dir this run was given.
func TestSecretsAuthWritesWhatWasTyped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	code, stdout, stderr := runAuth(t, emptyHome(t), typedAnswers(authTypedItem, authTypedKey), flagDirArg, dir)
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

	for _, secret := range []string{authTypedID, authTypedSecret, authTypedKey} {
		if strings.Contains(stdout+stderr, secret) {
			t.Fatalf("a credential was echoed back:\nstdout: %s\nstderr: %s", stdout, stderr)
		}
	}

	if strings.Contains(stderr, "visible") {
		t.Fatalf("a redirected stdin warned about visible typing:\n%s", stderr)
	}

	if want := "cairnos secrets pull --dir '" + dir + "'"; !strings.Contains(stdout, want) {
		t.Fatalf("stdout does not name the next step as %q:\n%s", want, stdout)
	}

	info, err := os.Stat(filepath.Join(dir, credentialsFileName))
	if err != nil {
		t.Fatalf("stat the credentials file: %v", err)
	}

	if info.Mode().Perm() != pullPerm {
		t.Fatalf("credentials file is mode %04o, want %04o", info.Mode().Perm(), pullPerm)
	}
}

// TestSecretsAuthCreatesAHomeThatIsNotThereYet covers a first run on a host
// with no CairnOS home: the answers still land, in a directory made for them.
func TestSecretsAuthCreatesAHomeThatIsNotThereYet(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "fresh-home")

	if code, _, stderr := runAuth(t, emptyHome(t), typedAnswers(authTypedItem, authTypedKey), flagDirArg, dir); code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if written := readCredentials(t, dir); !strings.Contains(written, assignment(bitwarden.EnvMasterKey, authTypedKey)) {
		t.Fatalf("the answers did not reach the new home's credentials file:\n%s", written)
	}
}

// TestSecretsAuthPromptsWithoutAHome covers an environment that names no home:
// with --dir there is nothing to resolve under one, so the missing home only
// means there is no chezmoi config to look at, and the prompts run.
func TestSecretsAuthPromptsWithoutAHome(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	code, _, stderr := runAuth(t, map[string]string{}, typedAnswers(authTypedItem, authTypedKey), flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if written := readCredentials(t, dir); !strings.Contains(written, assignment(bitwarden.EnvClientID, authTypedID)) {
		t.Fatalf("the typed client id did not reach the file:\n%s", written)
	}
}

// TestSecretsAuthPointsTheConfigAtChezmoi is the no-duplication rule: a chezmoi
// config that already holds the vault keys gets pointed at, and no second copy
// of them is written. The operator's own lines in config.toml survive the edit,
// and a pull through the new pointer reads the vault.
func TestSecretsAuthPointsTheConfigAtChezmoi(t *testing.T) {
	t.Parallel()

	env := chezmoiHome(t, chezmoiBody())
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")

	const operatorLines = "# host notes\n[secrets]\nbackend = \"env\"\n"

	writePullFile(t, configPath, operatorLines)

	code, stdout, stderr := runAuth(t, env, "", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stdout, chezmoiPointer) {
		t.Fatalf("stdout does not name the file config.toml now points at:\n%s", stdout)
	}

	for _, secret := range []string{chezmoiID, chezmoiSecret, chezmoiKey} {
		if strings.Contains(stdout+stderr, secret) {
			t.Fatalf("a chezmoi credential was printed:\n%s%s", stdout, stderr)
		}
	}

	assertAbsent(t, filepath.Join(dir, credentialsFileName), "a copied credentials file")

	if edited := runPullRead(t, configPath); !strings.HasPrefix(edited, operatorLines) {
		t.Fatalf("the operator's lines did not survive the edit:\n%s", edited)
	}

	assertPointer(t, configPath, env, chezmoiPointer)

	binDir := stageFakeBW(t, map[string]string{cairnosOneVar: pullValueOne})
	pullEnv := pathEnv(binDir)
	pullEnv[cli.HomeEnvName()] = env[cli.HomeEnvName()]

	if pullCode, _, pullErr := runEnvCLI(t, pullEnv, secretsCmd, pullSubcmd, flagDirArg, dir); pullCode != cli.ExitOK {
		t.Fatalf("pull through the pointer: exit = %d, want ok\nstderr: %s", pullCode, pullErr)
	}
}

// assertPointer loads config.toml through the real loader and checks the
// credentials_file it now carries.
func assertPointer(t *testing.T, configPath string, env map[string]string, want string) {
	t.Helper()

	cfg, err := config.LoadWith(configPath, environ.Map(env))
	if err != nil {
		t.Fatalf("the edited config does not load: %v", err)
	}

	if cfg.Secrets.Bitwarden == nil {
		t.Fatal("the edited config has no [secrets.bitwarden] block")
	}

	if got := cfg.Secrets.Bitwarden.CredentialsFile; got != want {
		t.Fatalf("credentials_file = %q, want %q", got, want)
	}
}

// TestSecretsAuthFindsChezmoiWhereChezmoiLooks covers the XDG rule: chezmoi's
// config sits under $XDG_CONFIG_HOME when that is an absolute path and under
// ~/.config otherwise. A pointer under the home is written as ~/, and one
// outside it stays absolute. Every case starts with no config.toml, so the
// command also creates one.
func TestSecretsAuthFindsChezmoiWhereChezmoiLooks(t *testing.T) {
	t.Parallel()

	cases := []struct {
		// stage writes the chezmoi config and returns the XDG_CONFIG_HOME value
		// and the pointer the command should write.
		stage func(t *testing.T, home string) (string, string)
		name  string
	}{
		{
			name: "no XDG_CONFIG_HOME reads ~/.config",
			stage: func(t *testing.T, home string) (string, string) {
				t.Helper()

				stageChezmoiConfig(t, filepath.Join(home, ".config"), chezmoiBody())

				return "", chezmoiPointer
			},
		},
		{
			name: "XDG_CONFIG_HOME under the home is written with ~/",
			stage: func(t *testing.T, home string) (string, string) {
				t.Helper()

				xdg := filepath.Join(home, "xdg")
				stageChezmoiConfig(t, xdg, chezmoiBody())

				return xdg, "~/xdg/chezmoi/chezmoi.toml"
			},
		},
		{
			name: "XDG_CONFIG_HOME outside the home stays absolute",
			stage: func(t *testing.T, _ string) (string, string) {
				t.Helper()

				xdg := t.TempDir()

				return xdg, stageChezmoiConfig(t, xdg, chezmoiBody())
			},
		},
		{
			name: "a relative XDG_CONFIG_HOME is ignored",
			stage: func(t *testing.T, home string) (string, string) {
				t.Helper()

				stageChezmoiConfig(t, filepath.Join(home, ".config"), chezmoiBody())

				return "relative/xdg", chezmoiPointer
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			home := t.TempDir()
			xdg, want := testCase.stage(t, home)

			env := map[string]string{cli.HomeEnvName(): home}
			if xdg != "" {
				env["XDG_CONFIG_HOME"] = xdg
			}

			dir := t.TempDir()

			if code, _, stderr := runAuth(t, env, "", flagDirArg, dir); code != cli.ExitOK {
				t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
			}

			assertPointer(t, filepath.Join(dir, "config.toml"), env, want)
			assertAbsent(t, filepath.Join(dir, credentialsFileName), "a copied credentials file")
		})
	}
}

// TestSecretsAuthNeedsAnItemBeforePointingAtChezmoi covers a chezmoi config
// holding the three credentials but no item. Pointing at it alone would set up
// a pull that fails on the missing item, so it is refused unless config.toml
// names the item, and a refusal leaves config.toml untouched.
func TestSecretsAuthNeedsAnItemBeforePointingAtChezmoi(t *testing.T) {
	t.Parallel()

	const itemLine = "[secrets.bitwarden]\nitem = \"" + pullItem + "\"\n"

	cases := []struct {
		name     string
		config   string
		wantCode int
	}{
		{name: "no item anywhere is refused", config: "", wantCode: cli.ExitFailure},
		{name: "an item in config.toml is enough", config: itemLine, wantCode: cli.ExitOK},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			env := chezmoiHome(t, "[data.secret]\n"+chezmoiKeys())
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.toml")

			if testCase.config != "" {
				writePullFile(t, configPath, testCase.config)
			}

			code, _, stderr := runAuth(t, env, "", flagDirArg, dir)
			if code != testCase.wantCode {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, testCase.wantCode, stderr)
			}

			assertAbsent(t, filepath.Join(dir, credentialsFileName), "a copied credentials file")

			if testCase.wantCode == cli.ExitOK {
				assertPointer(t, configPath, env, chezmoiPointer)

				return
			}

			if !strings.Contains(stderr, "no vault item") {
				t.Fatalf("stderr does not say the item is missing:\n%s", stderr)
			}

			assertAbsent(t, configPath, "a config.toml")
		})
	}
}

// TestSecretsAuthSkipsChezmoiInSystemMode covers --system under sudo: the
// chezmoi config found there belongs to whoever ran sudo, which the service
// user cannot read, so the prompts run and config.toml is left as written.
func TestSecretsAuthSkipsChezmoiInSystemMode(t *testing.T) {
	t.Parallel()

	env := chezmoiHome(t, chezmoiBody())
	dir := t.TempDir()
	credentialsPath := filepath.Join(t.TempDir(), credentialsFileName)
	configPath := filepath.Join(dir, "config.toml")
	configBody := "[secrets.bitwarden]\ncredentials_file = \"" + credentialsPath + "\"\n"

	writePullFile(t, configPath, configBody)

	code, stdout, stderr := runAuth(t, env, typedAnswers(authTypedItem, authTypedKey),
		flagDirArg, dir, flagSystemArg)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if written := runPullRead(t, credentialsPath); !strings.Contains(written, assignment(bitwarden.EnvClientID, authTypedID)) {
		t.Fatalf("the typed client id did not reach the system credentials file:\n%s", written)
	}

	if after := runPullRead(t, configPath); after != configBody {
		t.Fatalf("config.toml changed in system mode:\n%s", after)
	}

	if !strings.Contains(stdout, "cairnos secrets pull --dir '"+dir+"' --system") {
		t.Fatalf("stdout does not name the system-mode next step:\n%s", stdout)
	}
}

// TestSecretsAuthLeavesEnvironmentValuesOutOfTheFile covers a host that injects
// some of the values: each one stays in the environment, and the file holds
// only what was typed, so no value ends up in two places.
func TestSecretsAuthLeavesEnvironmentValuesOutOfTheFile(t *testing.T) {
	t.Parallel()

	env := emptyHome(t)
	env[bitwarden.EnvClientSecret] = "env-client-secret"

	dir := t.TempDir()
	typed := strings.Join([]string{authTypedItem, authTypedID, authTypedKey}, "\n") + "\n"

	code, _, stderr := runAuth(t, env, typed, flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	written := readCredentials(t, dir)
	if strings.Contains(written, bitwarden.EnvClientSecret) {
		t.Fatalf("the environment's value was copied into the file:\n%s", written)
	}

	if !strings.Contains(written, assignment(bitwarden.EnvMasterKey, authTypedKey)) {
		t.Fatalf("the typed values did not reach the file:\n%s", written)
	}

	if !strings.Contains(stderr, bitwarden.EnvClientSecret+": kept in the environment") {
		t.Fatalf("stderr does not say the value stays in the environment:\n%s", stderr)
	}
}

// TestSecretsAuthWritesNothingWhenTheEnvironmentHoldsEverything covers the
// host that injects all four: there is nothing left for a file to hold.
func TestSecretsAuthWritesNothingWhenTheEnvironmentHoldsEverything(t *testing.T) {
	t.Parallel()

	env := emptyHome(t)
	for _, name := range []string{
		bitwarden.EnvItem, bitwarden.EnvClientID, bitwarden.EnvClientSecret, bitwarden.EnvMasterKey,
	} {
		env[name] = "env-" + strings.ToLower(name)
	}

	dir := t.TempDir()

	code, stdout, stderr := runAuth(t, env, "", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stdout, "no credentials file was written") {
		t.Fatalf("stdout does not say nothing was written:\n%s", stdout)
	}

	assertAbsent(t, filepath.Join(dir, credentialsFileName), "a credentials file")
}

// TestSecretsAuthPromptsWhenChezmoiLacksTheKeys covers a chezmoi config kept
// for other dotfiles: it is not a credentials file, so the prompts run and
// config.toml is left alone.
func TestSecretsAuthPromptsWhenChezmoiLacksTheKeys(t *testing.T) {
	t.Parallel()

	env := chezmoiHome(t, "[data]\nname = \"ops\"\n\n[data.secret]\nbw_client_id = \""+chezmoiID+"\"\n")
	dir := t.TempDir()

	if code, _, stderr := runAuth(t, env, typedAnswers(authTypedItem, authTypedKey), flagDirArg, dir); code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if written := readCredentials(t, dir); !strings.Contains(written, assignment(bitwarden.EnvClientID, authTypedID)) {
		t.Fatalf("the typed client id did not reach the file:\n%s", written)
	}

	assertAbsent(t, filepath.Join(dir, "config.toml"), "a config.toml")
}

// TestSecretsAuthRefusesAChezmoiConfigOthersCanRead covers the pointer that
// would only move the failure: the pull refuses a credentials file anybody but
// its owner can read, so auth says so now and writes nothing, not even a copy.
func TestSecretsAuthRefusesAChezmoiConfigOthersCanRead(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	path := stageChezmoiConfig(t, filepath.Join(home, ".config"), chezmoiBody())

	if err := os.Chmod(path, 0o644); err != nil { // #nosec G302 -- the loose mode is the case
		t.Fatalf("loosen the chezmoi config: %v", err)
	}

	dir := t.TempDir()

	code, _, stderr := runAuth(t, map[string]string{cli.HomeEnvName(): home}, "", flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want failure", code)
	}

	if !strings.Contains(stderr, modeRefusal) {
		t.Fatalf("stderr does not name the mode rule:\n%s", stderr)
	}

	assertAbsent(t, filepath.Join(dir, credentialsFileName), "a credentials file")
	assertAbsent(t, filepath.Join(dir, "config.toml"), "a config.toml")
}

// TestSecretsAuthFailsOnAClosedStdin proves a closed stdin ends the run. A
// deploy script that reaches
// this command has no terminal behind it, so the run has to end rather than wait
// for a line that cannot arrive. With no item in the config there is no default
// to fall back on, so the first prompt is the one that fails.
func TestSecretsAuthFailsOnAClosedStdin(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	code, _, stderr := runAuth(t, emptyHome(t), "", flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want failure", code)
	}

	if !strings.Contains(stderr, bitwarden.EnvItem) {
		t.Fatalf("stderr does not name the variable that went unanswered:\n%s", stderr)
	}

	assertAbsent(t, filepath.Join(dir, credentialsFileName), "a credentials file")
}

// TestSecretsAuthTakesABlankItemFromTheConfigBlock covers the one default the
// item prompt has: the item config.toml already names.
func TestSecretsAuthTakesABlankItemFromTheConfigBlock(t *testing.T) {
	t.Parallel()

	const configuredItem = "configured-item"

	dir := t.TempDir()
	writePullFile(t, filepath.Join(dir, "config.toml"), "[secrets.bitwarden]\nitem = \""+configuredItem+"\"\n")

	code, _, stderr := runAuth(t, emptyHome(t), typedAnswers("", authTypedKey), flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stderr, "["+configuredItem+"]") {
		t.Fatalf("the item prompt did not offer the configured item:\n%s", stderr)
	}

	if written := readCredentials(t, dir); !strings.Contains(written, assignment(bitwarden.EnvItem, configuredItem)) {
		t.Fatalf("a blank item answer did not take the configured item:\n%s", written)
	}
}

// TestSecretsAuthRefusesKeyValueLinesAtAChezmoiPath covers a credentials_file
// naming a .toml file that is not there. KEY=VALUE lines written at that path
// would be read back as an empty chezmoi config, so nothing is written.
func TestSecretsAuthRefusesKeyValueLinesAtAChezmoiPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writePullFile(t, filepath.Join(dir, "config.toml"), "[secrets.bitwarden]\ncredentials_file = \"creds.toml\"\n")

	code, _, stderr := runAuth(t, emptyHome(t), typedAnswers(authTypedItem, authTypedKey), flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want failure", code)
	}

	if !strings.Contains(stderr, "chezmoi config that does not exist") {
		t.Fatalf("stderr does not explain the refusal:\n%s", stderr)
	}

	assertAbsent(t, filepath.Join(dir, "creds.toml"), "a KEY=VALUE file at the .toml path")
}

// TestSecretsAuthRefusesADanglingSymlink covers a credentials path that is a
// link to nothing. Writing the file would replace the link the operator put
// there, so the run stops with the link intact.
func TestSecretsAuthRefusesADanglingSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	link := filepath.Join(dir, credentialsFileName)

	if err := os.Symlink(filepath.Join(t.TempDir(), "gone.env"), link); err != nil {
		t.Fatalf("stage the dangling link: %v", err)
	}

	code, _, stderr := runAuth(t, emptyHome(t), typedAnswers(authTypedItem, authTypedKey), flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want failure", code)
	}

	if !strings.Contains(stderr, "symlink") {
		t.Fatalf("stderr does not name the dangling link:\n%s", stderr)
	}

	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced (stat error %v)", err)
	}
}

// TestSecretsAuthRefusesALineBreakInAnAnswer covers a piped answer holding a
// carriage return mid-value. The loader reads the file line by line, so the
// value would come back split; nothing is written.
func TestSecretsAuthRefusesALineBreakInAnAnswer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	code, _, stderr := runAuth(t, emptyHome(t), typedAnswers(authTypedItem, "pass\rword"), flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want failure", code)
	}

	if !strings.Contains(stderr, bitwarden.EnvMasterKey) {
		t.Fatalf("stderr does not name the value holding the break:\n%s", stderr)
	}

	assertAbsent(t, filepath.Join(dir, credentialsFileName), "a credentials file")
}

// stagedFieldValue fills a vault field the status check counts but never reads.
const stagedFieldValue = "one"

// TestSecretsAuthChecksAnExistingFileWithoutRewritingIt proves the
// credentials already on the host are proven against the vault here, rather
// than at the next deploy, and every problem is reported without the file
// being touched. The count is the one the pull would write.
func TestSecretsAuthChecksAnExistingFileWithoutRewritingIt(t *testing.T) {
	t.Parallel()

	usableFields := map[string]string{
		cairnosOneVar: stagedFieldValue,
		"CAIRNOS_TWO": "two",
		pullFieldOne:  pullValueOne,
	}

	cases := []struct {
		fields   map[string]string
		name     string
		body     string
		wantText []string
		mode     os.FileMode
		wantCode int
	}{
		{
			name:     "complete file opens the vault and reports the item",
			fields:   usableFields,
			body:     credentialsFixtureFor(pullItem),
			mode:     pullPerm,
			wantCode: cli.ExitOK,
			wantText: []string{pullItem, "2 variables", "locked"},
		},
		{
			name:     "a missing value is named",
			fields:   usableFields,
			body:     bitwarden.EnvItem + "=" + pullItem + "\n" + bitwarden.EnvClientID + "=id-1\n",
			mode:     pullPerm,
			wantCode: cli.ExitFailure,
			wantText: []string{bitwarden.EnvClientSecret, bitwarden.EnvMasterKey},
		},
		{
			name:     "a file others can read is refused",
			fields:   usableFields,
			body:     credentialsFixtureFor(pullItem),
			mode:     0o644,
			wantCode: cli.ExitFailure,
			wantText: []string{modeRefusal},
		},
		{
			name:     "an item the vault lacks is named",
			fields:   usableFields,
			body:     credentialsFixtureFor("absent-item"),
			mode:     pullPerm,
			wantCode: cli.ExitFailure,
			wantText: []string{"absent-item"},
		},
		{
			name:     "an item that gives the pull nothing fails as the pull would",
			fields:   map[string]string{humanField: humanValue},
			body:     credentialsFixtureFor(pullItem),
			mode:     pullPerm,
			wantCode: cli.ExitFailure,
			wantText: []string{"no field on the vault item"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			binDir := stageFakeBW(t, testCase.fields)

			dir := t.TempDir()
			path := filepath.Join(dir, credentialsFileName)
			writePullFile(t, path, testCase.body)

			if err := os.Chmod(path, testCase.mode); err != nil {
				t.Fatalf("set the credentials file mode: %v", err)
			}

			code, stdout, stderr := runAuth(t, pathEnv(binDir), "", flagDirArg, dir)
			if code != testCase.wantCode {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, testCase.wantCode, stdout, stderr)
			}

			for _, want := range testCase.wantText {
				if !strings.Contains(stdout+stderr, want) {
					t.Fatalf("output missing %q:\nstdout: %s\nstderr: %s", want, stdout, stderr)
				}
			}

			if after := runPullRead(t, path); after != testCase.body {
				t.Fatalf("the credentials file was rewritten:\n%s", after)
			}
		})
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

// TestSecretsAuthRoundTripsAwkwardSecrets is the reason for the wrap and for
// trimming only the line terminator. The loader strips one matching pair and
// processes no escapes, so a password wrapped in quotes would come back two
// characters short without the wrap, and one with edge spaces would lose them
// to a trim. The vault refuses either unlock.
func TestSecretsAuthRoundTripsAwkwardSecrets(t *testing.T) {
	t.Parallel()

	for _, awkward := range []string{"'it's \"quoted\"'", "  spaced  "} {
		t.Run(awkward, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()

			if code, _, stderr := runAuth(t, emptyHome(t), typedAnswers(authTypedItem, awkward), flagDirArg, dir); code != cli.ExitOK {
				t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
			}

			creds, err := bitwarden.LoadCredentials(filepath.Join(dir, credentialsFileName), func(string) string { return "" })
			if err != nil {
				t.Fatalf("load the credentials back: %v", err)
			}

			if creds.MasterKey != awkward {
				t.Fatalf("master key round-tripped as %q, want %q", creds.MasterKey, awkward)
			}
		})
	}
}
