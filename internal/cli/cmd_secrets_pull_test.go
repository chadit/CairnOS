package cli_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/bitwarden"
	"github.com/chadit/CairnOS/internal/cli"
)

// The vault item, the two fields, and the variables they render into. Spelled
// the way a host config would spell them.
const (
	pullItem     = "cairnos"
	pullFieldOne = "mattermost token"
	pullFieldTwo = "zai key"
	pullVarOne   = "CAIRNOS_BRIDGE_MATTERMOST_TOKEN"
	pullVarTwo   = "CAIRNOS_RUNNER_OPENAI_API_KEY"
	pullValueOne = "token-value"
	pullValueTwo = "key-value"
)

// pullPerm is the mode the credentials file and the rendered file both carry.
const pullPerm = 0o600

// The pull subcommand and the flag that points it at the system-mode paths.
const (
	pullSubcmd    = "pull"
	flagSystemArg = "--system"
)

// modeRefusal is the part of the loader's error that names the mode rule, which
// both commands report for a credentials file others can read.
const modeRefusal = "must be 0600"

// bwFakeName is the symlink to this test binary that stands in for the CLI, and
// also the dispatch signal: `secrets pull` resolves it through LookPath, so the
// child arrives with this as its argv[0]. The signal cannot be an environment
// variable, because the client builds its child's environment from a fixed
// allowlist and would strip one.
const bwFakeName = "bw"

// bwFakeSettingsFile sits beside the symlink and carries the item fixture, the
// version the fake reports, and the failures a case asks it to take, for the
// same reason.
const bwFakeSettingsFile = "fake.json"

// bwReadyPolls is how many polls the wait for `bw serve` lasts here. A child
// normally binds within two, so this is ample room on a busy host, and it is
// finite: a daemon that answers neither way ends the wait instead of hanging the
// run. Nothing longer helps, because a host dropping loopback traffic keeps
// dropping it for minutes.
const bwReadyPolls = 120

// unreachableAsset is the release URL the staged pin carries. Nothing serves it,
// so a case that reaches the installer fails there instead of downloading the
// real Bitwarden CLI from a unit test.
const unreachableAsset = "https://127.0.0.1:1/bw.zip"

// bwFakeSettings is what the fake reads out of that file: the item fixture, the
// version it claims, and the two failures a resolution case needs it to take.
type bwFakeSettings struct {
	ItemsFile    string `json:"items_file"`
	Version      string `json:"version"`
	FailStatus   bool   `json:"fail_status"`
	RefuseUnlock bool   `json:"refuse_unlock"`
}

// bwStatusCmd is the bw subcommand every vault read opens with.
const bwStatusCmd = "status"

// runFakeBW is the fake CLI. It answers --version, status and login, and serves
// the item fixture over HTTP for the daemon protocol.
func runFakeBW(argv []string) int {
	if len(argv) == 0 {
		return 2
	}

	// serve is dispatched before the settings read so the listener binds first.
	// Everything after the bind is invisible to the caller's readiness probe,
	// which is what keeps that wait off a stopwatch.
	if argv[0] == "serve" {
		return runFakeServe(argv)
	}

	settings, err := loadBWFakeSettings()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake bw: %v\n", err)

		return 3
	}

	switch argv[0] {
	case "--version":
		fmt.Println(settings.Version)

		return 0
	case bwStatusCmd:
		return runFakeStatus(settings)
	case "login":
		fmt.Println("logged-in")

		return 0
	default:
		return 2
	}
}

// runFakeStatus answers the first call every vault read makes, or fails the way
// a CLI that cannot reach the server does.
func runFakeStatus(settings bwFakeSettings) int {
	if settings.FailStatus {
		fmt.Fprintln(os.Stderr, "fake bw: cannot reach the server")

		return 1
	}

	fmt.Println(`{"status":"unlocked"}`)

	return 0
}

// runFakeServe binds a real port because this runs in a re-executed child
// process standing in for `bw serve`, which the pull reaches over TCP. It then
// answers the endpoints the pull drives.
func runFakeServe(argv []string) int {
	//nolint:noctx // this runs in the re-executed fake process, which has no context to cancel
	listener, err := net.Listen("tcp", "127.0.0.1:"+bwFlagValue(argv, "--port"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake bw: %v\n", err)

		return 1
	}

	settings, err := loadBWFakeSettings()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake bw: %v\n", err)

		return 3
	}

	server := &http.Server{Handler: bwFakeHandler(settings), ReadHeaderTimeout: bwFakeIdle}

	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return 1
	}

	return 0
}

// bwFakeIdle bounds the fake's own header read; it never needs to be short.
const bwFakeIdle = 30 * time.Second

// bwFakeHandler serves the item fixture and answers the lifecycle endpoints. A
// refused unlock answers with a status, which is what a wrong master password
// looks like and what tells a caller the binary itself is fine.
func bwFakeHandler(settings bwFakeSettings) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		if settings.RefuseUnlock && request.URL.Path == "/unlock" {
			http.Error(writer, "invalid master password", http.StatusBadRequest)

			return
		}

		if !strings.HasPrefix(request.URL.Path, "/list/object/items") {
			writeFakeBody(writer, []byte(`{"success":true}`))

			return
		}

		items, err := os.ReadFile(settings.ItemsFile) // #nosec G304 -- a fixture path
		if err != nil {
			http.Error(writer, "no items fixture", http.StatusInternalServerError)

			return
		}

		writeFakeBody(writer, items)
	})
}

// writeFakeBody answers with body, noting a failed write on the fake's stderr,
// which is where the parent test reads what went wrong in this process.
func writeFakeBody(writer http.ResponseWriter, body []byte) {
	if _, err := writer.Write(body); err != nil {
		fmt.Fprintf(os.Stderr, "fake bw: write response: %v\n", err)
	}
}

// loadBWFakeSettings reads the settings beside the symlink.
func loadBWFakeSettings() (bwFakeSettings, error) {
	path := filepath.Join(filepath.Dir(os.Args[0]), bwFakeSettingsFile)

	raw, err := os.ReadFile(path) // #nosec G304 -- a fixture path beside the fake
	if err != nil {
		return bwFakeSettings{}, fmt.Errorf("read %s: %w", path, err)
	}

	var settings bwFakeSettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		return bwFakeSettings{}, fmt.Errorf("parse %s: %w", path, err)
	}

	return settings, nil
}

// bwFlagValue pulls one flag's value out of the fake's own argv.
func bwFlagValue(argv []string, name string) string {
	for index := range argv {
		if argv[index] == name && index+1 < len(argv) {
			return argv[index+1]
		}
	}

	return ""
}

// pullConfig is a home with the block, the fields, and a credentials file, which
// is everything `secrets pull` needs besides the fake CLI.
func pullConfig(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	toml := "[secrets.bitwarden]\nitem = \"" + pullItem + "\"\n\n" +
		"[secrets.bitwarden.fields]\n" +
		pullVarOne + " = \"" + pullFieldOne + "\"\n" +
		pullVarTwo + " = \"" + pullFieldTwo + "\"\n"

	writePullFile(t, filepath.Join(dir, "config.toml"), toml)
	writePullFile(t, filepath.Join(dir, "bitwarden.env"), credentialsFixture())

	return dir
}

// credentialsFixture builds the three lines from the exported names, so no
// literal PASSWORD= assignment appears in this file for a secret scanner to
// read as a hardcoded value.
func credentialsFixture() string {
	return bitwarden.EnvClientID + "=id-1\n" +
		bitwarden.EnvClientSecret + "=sec-1\n" +
		bitwarden.EnvMasterKey + "=mk-1\n"
}

// hostNewerVersion is what the fake claims unless a case says otherwise. It is
// past any release this build could pin, so an ordinary case runs the bw the
// host has rather than the fallback.
const hostNewerVersion = "9999.12.31"

// stageFakeBW symlinks this test binary as bw and writes the item fixture. It
// returns the directory to put first on PATH, which is how the vault commands
// find it through LookPath.
func stageFakeBW(t *testing.T, fields map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	stageFakeBWAt(t, dir, fields, bwFakeSettings{Version: hostNewerVersion})

	return dir
}

// stageFakeBWAt stages the fake in a directory the case names, which is how a
// case puts one copy on PATH and another in the directory CairnOS keeps its own
// bw in. The settings carry the version and the failures; the item fixture and
// the symlink are the same either way.
func stageFakeBWAt(t *testing.T, dir string, fields map[string]string, settings bwFakeSettings) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("stage %s: %v", dir, err)
	}

	items := filepath.Join(dir, "items.json")
	settings.ItemsFile = items

	writePullFile(t, items, encodeItems(t, fields))
	writePullFile(t, filepath.Join(dir, bwFakeSettingsFile), encodeJSON(t, settings))

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}

	if linkErr := os.Symlink(self, filepath.Join(dir, bwFakeName)); linkErr != nil {
		t.Fatalf("link the fake bw: %v", linkErr)
	}
}

// Keys of the bw list response the fake answers with. The payload nests a data
// object inside the outer data key, the way the real CLI wraps its list output.
const (
	bwKeyName = "name"
	bwKeyData = "data"
)

// encodeItems renders the list response through encoding/json. Building it by
// concatenation would corrupt any value holding a quote, a backslash or a
// newline, which are exactly the values the escaping cases are about.
func encodeItems(t *testing.T, fields map[string]string) string {
	t.Helper()

	entries := make([]map[string]string, 0, len(fields))
	for name, value := range fields {
		entries = append(entries, map[string]string{bwKeyName: name, "value": value})
	}

	payload := map[string]any{
		"success": true,
		bwKeyData: map[string]any{
			bwKeyData: []map[string]any{{bwKeyName: pullItem, "fields": entries}},
		},
	}

	return encodeJSON(t, payload)
}

// encodeJSON renders one fixture value.
func encodeJSON(t *testing.T, value any) string {
	t.Helper()

	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode a fixture: %v", err)
	}

	return string(raw)
}

// writePullFile writes one fixture file at the given mode.
func writePullFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), pullPerm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// bothFields is the vault item the happy-path cases read.
func bothFields() map[string]string {
	return map[string]string{pullFieldOne: pullValueOne, pullFieldTwo: pullValueTwo}
}

// TestSecretsPullWithoutABlockIsANoOp covers the case every host without a
// vault takes. Deploy runs this unconditionally, so a non-zero exit here would
// stop all of them.
func TestSecretsPullWithoutABlockIsANoOp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writePullFile(t, filepath.Join(dir, "config.toml"), "[secrets]\nbackend = \"env\"\n")

	code, stdout, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stdout, "nothing to do") {
		t.Fatalf("stdout does not say the block is absent:\n%s", stdout)
	}
}

// TestSecretsPullUsesAnUnpinnedPlatformsOwnBW covers the platforms outside the
// two-entry pin table. There is no release to fall back to there, so the bw the
// host has is the only one, and the log says why it was taken on trust.
func TestSecretsPullUsesAnUnpinnedPlatformsOwnBW(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, bothFields())
	dir := pullConfig(t)

	code, stdout, stderr := runUnpinnedEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stderr, "no release is pinned for") {
		t.Fatalf("the log does not say the platform is unpinned:\n%s", stderr)
	}

	if !strings.Contains(stdout, pullVarOne) {
		t.Fatalf("the pull did not render the vault fields:\n%s", stdout)
	}
}

// TestSecretsPullWritesTheEnvFile covers the rendered file and the line the
// pull prints. The variable names belong in a deploy log; the values never do.
func TestSecretsPullWritesTheEnvFile(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, bothFields())
	dir := pullConfig(t)

	code, stdout, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	path := filepath.Join(dir, "secrets.env")

	body, err := os.ReadFile(path) // #nosec G304 -- the path is built by the test
	if err != nil {
		t.Fatalf("read the rendered file: %v", err)
	}

	rendered := string(body)
	for _, want := range []string{
		pullVarOne + "='" + pullValueOne + "'",
		pullVarTwo + "='" + pullValueTwo + "'",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered file missing %q:\n%s", want, rendered)
		}
	}

	if !strings.Contains(stdout, pullVarOne) || strings.Contains(stdout, pullValueOne) {
		t.Fatalf("stdout must name the variables and no value:\n%s", stdout)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the rendered file: %v", err)
	}

	if info.Mode().Perm() != pullPerm {
		t.Fatalf("rendered file is mode %04o, want %04o", info.Mode().Perm(), pullPerm)
	}
}

// TestSecretsPullIsByteStable proves two runs against an unchanged vault produce
// identical files, so a diff in the deployed file always means something moved.
func TestSecretsPullIsByteStable(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, bothFields())
	dir := pullConfig(t)

	path := filepath.Join(dir, "secrets.env")
	first := runPullAndRead(t, binDir, dir, path)

	if second := runPullAndRead(t, binDir, dir, path); second != first {
		t.Fatalf("two pulls differ:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestSecretsPullFollowsAChangedVaultValue is the other half of byte stability.
// Identical bytes are only worth pinning if a moved value moves them: a pull
// that rendered the previous value would leave the service on a rotated secret
// it can no longer use.
func TestSecretsPullFollowsAChangedVaultValue(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, bothFields())
	dir := pullConfig(t)

	path := filepath.Join(dir, "secrets.env")
	before := runPullAndRead(t, binDir, dir, path)

	const rotatedValue = "rotated-token-value"

	restageBWFields(t, binDir, map[string]string{
		pullFieldOne: rotatedValue,
		pullFieldTwo: pullValueTwo,
	})

	after := runPullAndRead(t, binDir, dir, path)
	if after == before {
		t.Fatalf("the rotated value did not change the file:\n%s", after)
	}

	if !strings.Contains(after, pullVarOne+"='"+rotatedValue+"'") {
		t.Fatalf("the file does not carry the rotated value:\n%s", after)
	}

	if strings.Contains(after, pullVarOne+"='"+pullValueOne+"'") {
		t.Fatalf("the file still carries the previous value:\n%s", after)
	}
}

// restageBWFields rewrites the item fixture a staged fake already serves, which
// is how a case moves a vault value between two pulls. The symlink and the
// settings file stay as they were.
func restageBWFields(t *testing.T, dir string, fields map[string]string) {
	t.Helper()

	writePullFile(t, filepath.Join(dir, "items.json"), encodeItems(t, fields))
}

// runPullAndRead runs one successful pull and returns the rendered file.
func runPullAndRead(t *testing.T, binDir, dir, path string) string {
	t.Helper()

	code, _, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	body, err := os.ReadFile(path) // #nosec G304 -- the path is built by the test
	if err != nil {
		t.Fatalf("read the rendered file: %v", err)
	}

	return string(body)
}

// TestSecretsPullDryRunWritesNothing covers the dry run: an operator checks the mapping
// against the vault before a deploy touches the file the service reads.
func TestSecretsPullDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, bothFields())
	dir := pullConfig(t)

	code, stdout, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir, "--dry-run")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stdout, "would write") {
		t.Fatalf("stdout does not report a dry run:\n%s", stdout)
	}

	if _, err := os.Stat(filepath.Join(dir, "secrets.env")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a dry run created the file (stat error %v)", err)
	}
}

// TestSecretsPullNamesEveryMissingField covers missing fields end to end: the run fails,
// names everything to add, and leaves no file behind for the service to read.
func TestSecretsPullNamesEveryMissingField(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, map[string]string{pullFieldOne: pullValueOne})
	dir := pullConfig(t)

	code, _, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want failure", code)
	}

	if !strings.Contains(stderr, pullFieldTwo) {
		t.Fatalf("stderr does not name the missing field:\n%s", stderr)
	}

	if _, err := os.Stat(filepath.Join(dir, "secrets.env")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a failed pull wrote the file (stat error %v)", err)
	}
}

// TestSecretsPullLeavesNoTempFileBehind drives a failure that reaches the write
// itself. Making the destination a directory is what fails the rename, so the
// cleanup path this test is about actually runs; a failure earlier in the pull
// would never enter writeFileAtomic and the test would prove nothing.
func TestSecretsPullLeavesNoTempFileBehind(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, bothFields())
	dir := pullConfig(t)

	if err := os.Mkdir(filepath.Join(dir, "secrets.env"), 0o750); err != nil {
		t.Fatalf("stage a directory where the file goes: %v", err)
	}

	if code, _, _ := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir); code != cli.ExitFailure {
		t.Fatalf("exit = %d, want failure when the rename cannot land", code)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the home: %v", err)
	}

	for _, entry := range entries {
		if entry.Name() != "secrets.env" && strings.HasPrefix(entry.Name(), "secrets.env") {
			t.Fatalf("a failed write left %s behind", entry.Name())
		}
	}
}

// TestSecretsPullQuotesValuesForBothReaders covers the escaping half of the
// rendered file. A raw value with a space would set half a variable and run the
// rest as a command when the launchd preamble sources it, so each one is single
// quoted. This sources the file the way that preamble does and compares what the
// shell made of it.
func TestSecretsPullQuotesValuesForBothReaders(t *testing.T) {
	t.Parallel()

	hostile := map[string]string{
		pullFieldOne: "two words $(touch /tmp/pwned) `id` done",
		pullFieldTwo: "it's got a quote and a \\backslash",
	}

	binDir := stageFakeBW(t, hostile)
	dir := pullConfig(t)

	if code, _, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir); code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	readBack := sourceEnvFile(t, filepath.Join(dir, "secrets.env"))

	if readBack[pullVarOne] != hostile[pullFieldOne] {
		t.Fatalf("%s round-tripped as %q, want %q", pullVarOne, readBack[pullVarOne], hostile[pullFieldOne])
	}

	if readBack[pullVarTwo] != hostile[pullFieldTwo] {
		t.Fatalf("%s round-tripped as %q, want %q", pullVarTwo, readBack[pullVarTwo], hostile[pullFieldTwo])
	}
}

// sourceEnvFile reads the generated file back through /bin/sh under `set -a`,
// which is exactly what the launchd preamble does, and returns what the shell
// ended up with.
func sourceEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()

	script := "set -a; . " + path + "; set +a; " +
		"printf '%s\\n' \"${" + pullVarOne + "}\" \"${" + pullVarTwo + "}\""

	out, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", script).Output() // #nosec G204 -- a test-built script over a test-owned path
	if err != nil {
		t.Fatalf("source the generated file: %v", err)
	}

	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("sourcing produced %d lines, want 2:\n%s", len(lines), out)
	}

	return map[string]string{pullVarOne: lines[0], pullVarTwo: lines[1]}
}

// TestSecretsPullRefusesAValueWithANewline covers the one shape neither reader
// can carry: both parse line by line, so the remainder would be read as a second
// assignment.
func TestSecretsPullRefusesAValueWithANewline(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, map[string]string{
		pullFieldOne: "first line\nsecond line",
		pullFieldTwo: pullValueTwo,
	})
	dir := pullConfig(t)

	code, _, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want failure", code)
	}

	if !strings.Contains(stderr, pullVarOne) || !strings.Contains(stderr, "newline") {
		t.Fatalf("stderr does not name the offending variable:\n%s", stderr)
	}

	if _, err := os.Stat(filepath.Join(dir, "secrets.env")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused value still wrote the file (stat error %v)", err)
	}
}

// TestSecretsPullTargetsWhereTheUnitReads covers the system-mode split. A system
// unit loads /etc/cairnos, never the home, so a pull resolving against the home
// would write a file nothing reads, and the dash on the unit line keeps that
// silent. It is a dry run, so nothing under /etc is touched.
func TestSecretsPullTargetsWhereTheUnitReads(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, bothFields())
	dir := pullConfigWithCredentials(t)

	cases := []struct {
		name string
		want string
		args []string
	}{
		{name: "user mode writes into the home", want: filepath.Join(dir, "secrets.env")},
		{name: "system mode writes under /etc", want: "/etc/cairnos/secrets.env", args: []string{flagSystemArg}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			argv := append([]string{secretsCmd, pullSubcmd, flagDirArg, dir, flagDryRunArg}, testCase.args...)

			code, stdout, stderr := runEnvCLI(t, pathEnv(binDir), argv...)
			if code != cli.ExitOK {
				t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
			}

			if !strings.Contains(stdout, testCase.want) {
				t.Fatalf("stdout does not name %s:\n%s", testCase.want, stdout)
			}
		})
	}
}

// pullConfigWithCredentials is pullConfig with an absolute credentials_file, so
// a case about the generated file's location does not also have to satisfy the
// credentials lookup from wherever the mode puts it.
func pullConfigWithCredentials(t *testing.T) string {
	t.Helper()

	dir := pullConfig(t)
	creds := filepath.Join(dir, "bitwarden.env")

	toml := "[secrets.bitwarden]\nitem = \"" + pullItem + "\"\n" +
		"credentials_file = \"" + creds + "\"\n\n" +
		"[secrets.bitwarden.fields]\n" +
		pullVarOne + " = \"" + pullFieldOne + "\"\n" +
		pullVarTwo + " = \"" + pullFieldTwo + "\"\n"

	writePullFile(t, filepath.Join(dir, "config.toml"), toml)

	return dir
}

// The inferred-field cases: a field already named like the variable it fills, a
// field named for a human, and the variable a mapping points at.
const (
	// cairnosOneVar is a second inferred field name, used where a case needs one
	// short enough to read inside a table row.
	cairnosOneVar = "CAIRNOS_ONE"

	inferredVar   = "CAIRNOS_SECRET_WEBHOOK"
	inferredValue = "webhook-value"
	humanField    = "some human name"
	humanValue    = "human-value"
)

// writeBitwardenConfig stages a home whose config holds the given
// [secrets.bitwarden] body, plus the credentials file every pull needs.
func writeBitwardenConfig(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()

	writePullFile(t, filepath.Join(dir, "config.toml"), body)
	writePullFile(t, filepath.Join(dir, "bitwarden.env"), credentialsFixture())

	return dir
}

// TestSecretsPullInfersFieldsNamedLikeVariables proves a field whose name
// is already the variable name needs no mapping, which is what keeps a host's
// config from listing the same string twice. A field named for a human is left
// alone, because nothing says what variable it would fill.
func TestSecretsPullInfersFieldsNamedLikeVariables(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, map[string]string{
		inferredVar: inferredValue,
		humanField:  humanValue,
	})

	dir := writeBitwardenConfig(t, "[secrets.bitwarden]\nitem = \""+pullItem+"\"\n")

	code, stdout, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	rendered := runPullRead(t, filepath.Join(dir, "secrets.env"))

	if !strings.Contains(rendered, inferredVar+"='"+inferredValue+"'") {
		t.Fatalf("the inferred field is missing from the rendered file:\n%s", rendered)
	}

	if strings.Contains(rendered, humanValue) {
		t.Fatalf("a field named for a human was written anyway:\n%s", rendered)
	}

	if !strings.Contains(stdout, inferredVar) {
		t.Fatalf("stdout does not name the inferred variable:\n%s", stdout)
	}
}

// TestSecretsPullPrefersTheMappedField covers the collision half of field inference: an
// operator who wrote a mapping meant it, so the table beats the field that
// happens to share the variable's name.
func TestSecretsPullPrefersTheMappedField(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, map[string]string{
		inferredVar: inferredValue,
		humanField:  humanValue,
	})

	dir := writeBitwardenConfig(t, "[secrets.bitwarden]\nitem = \""+pullItem+"\"\n\n"+
		"[secrets.bitwarden.fields]\n"+inferredVar+" = \""+humanField+"\"\n")

	if code, _, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir); code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	rendered := runPullRead(t, filepath.Join(dir, "secrets.env"))

	if !strings.Contains(rendered, inferredVar+"='"+humanValue+"'") {
		t.Fatalf("the mapping did not win the collision:\n%s", rendered)
	}
}

// TestSecretsPullFailsWhenTheItemCarriesNothing covers the other inference edge: an
// item with no variable-named field and no table is a configuration the pull
// cannot act on, and the message has to name the item that was read.
func TestSecretsPullFailsWhenTheItemCarriesNothing(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, map[string]string{humanField: humanValue})

	dir := writeBitwardenConfig(t, "[secrets.bitwarden]\nitem = \""+pullItem+"\"\n")

	code, _, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want failure", code)
	}

	if !strings.Contains(stderr, pullItem) {
		t.Fatalf("stderr does not name the item that carried nothing:\n%s", stderr)
	}
}

// TestSecretsPullTakesTheItemFromTheCredentialsFile proves a host whose
// credentials file names the item and whose config.toml does not still pulls.
func TestSecretsPullTakesTheItemFromTheCredentialsFile(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, map[string]string{inferredVar: inferredValue})

	dir := t.TempDir()
	writePullFile(t, filepath.Join(dir, "config.toml"), "[secrets.bitwarden]\n")
	writePullFile(t, filepath.Join(dir, "bitwarden.env"),
		bitwarden.EnvItem+"="+pullItem+"\n"+credentialsFixture())

	if code, _, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir); code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	rendered := runPullRead(t, filepath.Join(dir, "secrets.env"))
	if !strings.Contains(rendered, inferredVar) {
		t.Fatalf("the pull did not use the item from the credentials file:\n%s", rendered)
	}
}

// TestSecretsPullReadsACredentialsFileWrittenUnderTilde covers a
// credentials_file written as ~/..., the form a chezmoi pointer takes. The
// environment carries no BW_ values, so the pull can only succeed by expanding
// ~/ to the staged home and reading the file there.
func TestSecretsPullReadsACredentialsFileWrittenUnderTilde(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, bothFields())

	home := t.TempDir()
	writePullFile(t, filepath.Join(home, "bitwarden.env"), credentialsFixture())

	dir := t.TempDir()
	writePullFile(t, filepath.Join(dir, "config.toml"),
		"[secrets.bitwarden]\nitem = \""+pullItem+"\"\n"+
			"credentials_file = \"~/bitwarden.env\"\n\n"+
			"[secrets.bitwarden.fields]\n"+pullVarOne+" = \""+pullFieldOne+"\"\n")

	env := pathEnv(binDir)
	env[cli.HomeEnvName()] = home

	if code, _, stderr := runEnvCLI(t, env, secretsCmd, "pull", flagDirArg, dir); code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	rendered := runPullRead(t, filepath.Join(dir, "secrets.env"))
	if !strings.Contains(rendered, pullVarOne+"='"+pullValueOne+"'") {
		t.Fatalf("the pull did not render the mapped field:\n%s", rendered)
	}
}

// TestSecretsPullWarnsAboutImpliedVariables covers the implied-variable
// warning: the host will want a bot token and an API key at run time, and the
// item carries neither. The pull says so and still writes, because a value may
// be in the hand-written env file beside the generated one. The warning names
// the shared default an operator should add and mentions the override
// separately.
func TestSecretsPullWarnsAboutImpliedVariables(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, map[string]string{inferredVar: inferredValue})

	body := "[secrets.bitwarden]\nitem = \"" + pullItem + "\"\n\n" +
		"[bridge.mattermost]\nurl = \"https://mm.example.com\"\n\n" +
		"[bridge.mattermost.identity.ops]\nusername = \"ops-agent\"\nworkspace = \"default\"\n\n" +
		"[runner.openai]\nmodel = \"gpt-4.1\"\nbase_url = \"https://api.example.com/v1\"\n"

	dir := writeBitwardenConfig(t, body)

	code, _, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	for _, want := range []string{"CAIRNOS_BRIDGE_MATTERMOST_TOKEN", "CAIRNOS_RUNNER_OPENAI_API_KEY"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("the warning does not name %s:\n%s", want, stderr)
		}
	}

	if !strings.Contains(stderr, "CAIRNOS_BRIDGE_MATTERMOST_<ID>_TOKEN") {
		t.Fatalf("the warning does not mention the per-identity override:\n%s", stderr)
	}
}

// TestSecretsPullWarnsWithTheRunnerBlockName covers the runner half of that warning
// under a block name of its own. The variable named is the one that runner
// reads first, which is what an operator has to add to the item.
func TestSecretsPullWarnsWithTheRunnerBlockName(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, map[string]string{inferredVar: inferredValue})

	body := "[secrets.bitwarden]\nitem = \"" + pullItem + "\"\n\n" +
		"[runner.zai]\ntype = \"openai\"\nmodel = \"glm\"\nbase_url = \"https://api.z.ai/v1\"\n"

	dir := writeBitwardenConfig(t, body)

	code, _, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stderr, "CAIRNOS_RUNNER_ZAI_API_KEY") {
		t.Fatalf("the warning does not name the block's own key:\n%s", stderr)
	}
}

// TestSecretsPullIsQuietWhenTheItemCarriesTheIdentityToken covers the other
// side of the Mattermost rule: a host whose vault already holds the
// per-identity name needs no change, so warning about the shared default would
// send the operator to add a second copy of a token they have.
func TestSecretsPullIsQuietWhenTheItemCarriesTheIdentityToken(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, map[string]string{"CAIRNOS_BRIDGE_MATTERMOST_OPS_TOKEN": "token-value"})

	body := "[secrets.bitwarden]\nitem = \"" + pullItem + "\"\n\n" +
		"[bridge.mattermost]\nurl = \"https://mm.example.com\"\n\n" +
		"[bridge.mattermost.identity.ops]\nusername = \"ops-agent\"\nworkspace = \"default\"\n"

	dir := writeBitwardenConfig(t, body)

	code, _, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if strings.Contains(stderr, "CAIRNOS_BRIDGE_MATTERMOST_TOKEN") {
		t.Fatalf("the pull warned about a token the item already carries:\n%s", stderr)
	}
}

// TestSecretsPullNamesAnIdentityTokenEnv covers the explicit key: an identity
// that names its own variable is warned about under that name, not the default.
// TestSecretsPullNamesAnIdentityTokenEnv covers an explicit token_env, which
// nothing falls back from, so an override sentence would tell the operator about
// a name this config never reads.
func TestSecretsPullNamesAnIdentityTokenEnv(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, map[string]string{inferredVar: inferredValue})

	body := "[secrets.bitwarden]\nitem = \"" + pullItem + "\"\n\n" +
		"[bridge.mattermost]\nurl = \"https://mm.example.com\"\n\n" +
		"[bridge.mattermost.identity.ops]\nusername = \"ops-agent\"\nworkspace = \"default\"\n" +
		"token_env = \"CAIRNOS_SECOND_BOT_TOKEN\"\n"

	dir := writeBitwardenConfig(t, body)

	code, _, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stderr, "CAIRNOS_SECOND_BOT_TOKEN") {
		t.Fatalf("the warning does not name the identity's own variable:\n%s", stderr)
	}

	if strings.Contains(stderr, "CAIRNOS_BRIDGE_MATTERMOST_<ID>_TOKEN") {
		t.Fatalf("the warning offered an override an explicit token_env rules out:\n%s", stderr)
	}
}

// TestSecretsPullIsQuietAboutALocalRunner is the other side of that warning: a runner
// pointed at a server on this side of the network reads no key, so warning about
// one would train an operator to ignore the warnings that matter.
func TestSecretsPullIsQuietAboutALocalRunner(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, map[string]string{inferredVar: inferredValue})

	body := "[secrets.bitwarden]\nitem = \"" + pullItem + "\"\n\n" +
		"[runner.local]\ntype = \"openai\"\nmodel = \"gemma\"\nbase_url = \"http://127.0.0.1:8089/v1\"\n"

	dir := writeBitwardenConfig(t, body)

	code, _, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if strings.Contains(stderr, "CAIRNOS_RUNNER_OPENAI_API_KEY") {
		t.Fatalf("a local runner produced an API-key warning:\n%s", stderr)
	}
}

// runPullRead returns the rendered file for a case that already checked the exit
// code.
func runPullRead(t *testing.T, path string) string {
	t.Helper()

	body, err := os.ReadFile(path) // #nosec G304 -- the path is built by the test
	if err != nil {
		t.Fatalf("read the rendered file: %v", err)
	}

	return string(body)
}

// TestSecretsPullRendersTheBytesSystemdParses pins the generated format. The
// launchd side is covered by sourcing the file through /bin/sh; systemd is the
// only platform this actually ships to and it is not present in a test run, so
// its half is pinned as exact bytes. A single-quoted run is taken literally, so
// a plain value comes back with the wrap removed.
func TestSecretsPullRendersTheBytesSystemdParses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value string
		want  string
	}{
		{name: "a plain value", value: "abc", want: `CAIRNOS_ONE='abc'`},
		{name: "a value with spaces", value: "two words", want: `CAIRNOS_ONE='two words'`},
		{name: "a backslash single quotes do not let escape", value: `a\b`, want: `CAIRNOS_ONE='a\b'`},
		{name: "a value that looks like a command nothing substitutes", value: "$(id)", want: `CAIRNOS_ONE='$(id)'`},
		{name: "a value with double quotes", value: `say "hi"`, want: `CAIRNOS_ONE='say "hi"'`},
		{name: "a single quote, closed and reopened around an escaped one", value: "it's", want: `CAIRNOS_ONE='it'\''s'`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			binDir := stageFakeBW(t, map[string]string{cairnosOneVar: testCase.value})
			dir := writeBitwardenConfig(t, "[secrets.bitwarden]\nitem = \""+pullItem+"\"\n")

			if code, _, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir); code != cli.ExitOK {
				t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
			}

			rendered := runPullRead(t, filepath.Join(dir, "secrets.env"))
			if !strings.Contains(rendered, testCase.want+"\n") {
				t.Fatalf("rendered file does not carry the line %s:\n%s", testCase.want, rendered)
			}
		})
	}
}

// TestSecretsPullSystemModeReadsTheSystemConfig covers the documented sudo form.
// Without --dir, --system has to read the system install's config: reading the
// invoking user's home instead finds no block, prints "nothing to do", and exits
// 0, so a deploy reports success having written nothing. The user home is staged
// with a block too, so a fallback there fails on missing credentials.
func TestSecretsPullSystemModeReadsTheSystemConfig(t *testing.T) {
	t.Parallel()

	if _, err := os.Stat(systemConfigPath); err == nil {
		t.Skipf("%s exists on this host, so its contents would decide the case", systemConfigPath)
	}

	home := t.TempDir()

	userDir := filepath.Join(home, ".cairnos")
	if err := os.MkdirAll(userDir, 0o750); err != nil {
		t.Fatalf("stage the user home: %v", err)
	}

	writePullFile(t, filepath.Join(userDir, "config.toml"),
		"[secrets.bitwarden]\nitem = \""+pullItem+"\"\n")

	code, stdout, stderr := runEnvCLI(t, map[string]string{cli.HomeEnvName(): home},
		secretsCmd, "pull", "--system")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	// The whole line, because stdout also carries the user home's path in the
	// failure this case is written against, and the path alone would match it.
	want := "secrets pull: no [secrets.bitwarden] block in " + systemConfigPath + ", nothing to do\n"
	if !strings.Contains(stdout, want) {
		t.Fatalf("stdout does not carry %q:\n%s", want, stdout)
	}
}

// TestSecretsPullReportsATargetItCannotWrite covers the deploy that would
// otherwise look done. The rendered file goes through a temp file in the target
// directory, so a directory the unit owns and this user does not has to end the
// pull nonzero rather than exit clean having written nothing.
func TestSecretsPullReportsATargetItCannotWrite(t *testing.T) {
	t.Parallel()

	requireNotRoot(t)

	binDir := stageFakeBW(t, bothFields())
	dir := t.TempDir()
	sealed := filepath.Join(dir, "sealed")

	if err := os.Mkdir(sealed, 0o500); err != nil {
		t.Fatalf("seal the target directory: %v", err)
	}

	target := filepath.Join(sealed, "secrets.env")

	writePullFile(t, filepath.Join(dir, "config.toml"),
		"[secrets.bitwarden]\nitem = \""+pullItem+"\"\n"+
			"env_file = \""+target+"\"\n\n"+
			"[secrets.bitwarden.fields]\n"+
			pullVarOne+" = \""+pullFieldOne+"\"\n")
	writePullFile(t, filepath.Join(dir, "bitwarden.env"), credentialsFixture())

	code, stdout, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want a failure\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	if !strings.Contains(stderr, sealed) {
		t.Fatalf("the failure does not name the directory it could not write:\n%s", stderr)
	}

	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a failed pull left something at %s (stat error %v)", target, err)
	}
}

// systemConfigPath is where a system install keeps its config, which is what the
// documented `sudo cairnos secrets pull --system` has to read.
const systemConfigPath = "/var/lib/cairnos/config.toml"

// TestSecretsPullNamesTheAuthCommandWhenCredentialsAreMissing covers the cold
// start: a pull that finds no credentials file says which command creates one,
// with the --dir and --system the pull ran with. A file that exists gets no such
// hint, whether it lacks values or is refused, since `secrets auth` checks an
// existing file rather than rewriting it and would only repeat the error.
func TestSecretsPullNamesTheAuthCommandWhenCredentialsAreMissing(t *testing.T) {
	t.Parallel()

	const (
		missingFrom  = "is missing from"
		noSecretKeys = "has no [data.secret]"
	)

	cases := []struct {
		// stage writes the credentials file, if any, and returns the
		// credentials_file line config.toml carries.
		stage    func(t *testing.T, dir string) string
		name     string
		wantText string
		args     []string
		wantHint bool
	}{
		{
			name:     "no KEY=VALUE file and nothing in the environment",
			stage:    func(*testing.T, string) string { return "" },
			wantText: missingFrom,
			wantHint: true,
		},
		{
			name: "a system-mode pull names the system-mode command",
			stage: func(_ *testing.T, dir string) string {
				return "credentials_file = \"" + filepath.Join(dir, "absent.env") + "\"\n"
			},
			args:     []string{flagSystemArg},
			wantText: missingFrom,
			wantHint: true,
		},
		{
			name: "a chezmoi config without the keys gets no hint",
			stage: func(t *testing.T, dir string) string {
				t.Helper()

				path := filepath.Join(dir, "chezmoi.toml")
				writePullFile(t, path, "[data.secret]\nbw_item_name = \""+pullItem+"\"\n")

				return "credentials_file = \"" + path + "\"\n"
			},
			wantText: noSecretKeys,
		},
		{
			name: "an absent chezmoi config gets no hint",
			stage: func(_ *testing.T, dir string) string {
				return "credentials_file = \"" + filepath.Join(dir, "absent.toml") + "\"\n"
			},
			wantText: noSecretKeys,
		},
		{
			name: "a KEY=VALUE file missing a value gets no hint",
			stage: func(t *testing.T, dir string) string {
				t.Helper()

				writePullFile(t, filepath.Join(dir, "bitwarden.env"), bitwarden.EnvClientID+"=id-1\n")

				return ""
			},
			wantText: missingFrom,
		},
		{
			name: "a file others can read gets no hint",
			stage: func(t *testing.T, dir string) string {
				t.Helper()

				path := filepath.Join(dir, "bitwarden.env")
				writePullFile(t, path, credentialsFixture())

				if err := os.Chmod(path, 0o644); err != nil { // #nosec G302 -- the loose mode is the case
					t.Fatalf("loosen the credentials file: %v", err)
				}

				return ""
			},
			wantText: modeRefusal,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			pointer := testCase.stage(t, dir)

			writePullFile(t, filepath.Join(dir, "config.toml"),
				"[secrets.bitwarden]\nitem = \""+pullItem+"\"\n"+pointer)

			args := append([]string{secretsCmd, pullSubcmd, flagDirArg, dir}, testCase.args...)

			code, _, stderr := runEnvCLI(t, map[string]string{}, args...)
			if code != cli.ExitFailure {
				t.Fatalf("exit = %d, want failure", code)
			}

			if !strings.Contains(stderr, testCase.wantText) {
				t.Fatalf("stderr does not carry %q:\n%s", testCase.wantText, stderr)
			}

			if !testCase.wantHint {
				if strings.Contains(stderr, "secrets auth") {
					t.Fatalf("stderr points at secrets auth for a failure it cannot fix:\n%s", stderr)
				}

				return
			}

			flags := append([]string{flagDirArg, "'" + dir + "'"}, testCase.args...)

			if hint := "; run cairnos secrets auth " + strings.Join(flags, " ") + " to set them up"; !strings.Contains(stderr, hint) {
				t.Fatalf("stderr does not carry %q:\n%s", hint, stderr)
			}
		})
	}
}
