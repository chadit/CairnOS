package cli_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	pullItem     = "chezmoi"
	pullFieldOne = "mattermost token"
	pullFieldTwo = "zai key"
	pullVarOne   = "CAIRNOS_BRIDGE_MATTERMOST_TOKEN"
	pullVarTwo   = "CAIRNOS_RUNNER_OPENAI_API_KEY"
	pullValueOne = "token-value"
	pullValueTwo = "key-value"
)

// pullPerm is the mode the credentials file and the rendered file both carry.
const pullPerm = 0o600

// bwFakeName is the symlink to this test binary that stands in for the CLI, and
// also the dispatch signal: `secrets pull` resolves it through LookPath, so the
// child arrives with this as its argv[0]. The signal cannot be an environment
// variable, because the client builds its child's environment from a fixed
// allowlist and would strip one.
const bwFakeName = "bw"

// bwFakeSettingsFile sits beside the symlink and carries the item fixture path,
// for the same reason.
const bwFakeSettingsFile = "fake.json"

// unreachableAsset is the release URL the staged pin carries. Nothing serves it,
// so a case that reaches the installer fails there instead of downloading the
// real Bitwarden CLI from a unit test.
const unreachableAsset = "https://127.0.0.1:1/bw.zip"

// strangerDigest is a well-formed sha256 no staged binary matches, which is how
// a case puts the host a pin behind.
const strangerDigest = "0000000000000000000000000000000000000000000000000000000000000000"

// bwDigestRecord is the suffix bwinstall records an installed digest under,
// beside the binary it wrote.
const bwDigestRecord = ".cairnos-sha256"

// bwFakeSettings is what the fake reads out of that file.
type bwFakeSettings struct {
	ItemsFile string `json:"items_file"`
}

// runFakeBW is the fake CLI. It answers status and login, and serves the item
// fixture over HTTP for the daemon protocol.
func runFakeBW(argv []string) int {
	if len(argv) == 0 {
		return 2
	}

	switch argv[0] {
	case "status":
		fmt.Println(`{"status":"unlocked"}`)

		return 0
	case "login":
		fmt.Println("logged-in")

		return 0
	case "serve":
		return runFakeServe(argv)
	default:
		return 2
	}
}

// runFakeServe answers the endpoints the pull drives.
func runFakeServe(argv []string) int {
	settings, err := loadBWFakeSettings()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake bw: %v\n", err)

		return 3
	}

	server := &http.Server{
		Addr:              "127.0.0.1:" + bwFlagValue(argv, "--port"),
		Handler:           bwFakeHandler(settings),
		ReadHeaderTimeout: bwFakeIdle,
	}

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return 1
	}

	return 0
}

// bwFakeIdle bounds the fake's own header read; it never needs to be short.
const bwFakeIdle = 30 * time.Second

// bwFakeHandler serves the item fixture and answers the lifecycle endpoints.
func bwFakeHandler(settings bwFakeSettings) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")

		if !strings.HasPrefix(request.URL.Path, "/list/object/items") {
			_, _ = writer.Write([]byte(`{"success":true}`))

			return
		}

		items, err := os.ReadFile(settings.ItemsFile) // #nosec G304 -- a fixture path
		if err != nil {
			http.Error(writer, "no items fixture", http.StatusInternalServerError)

			return
		}

		_, _ = writer.Write(items)
	})
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

// stageFakeBW symlinks this test binary as bw and writes the item fixture. It
// returns the directory to put first on PATH, which is how `secrets pull` finds
// it through LookPath.
//
// runCLI pins the release to this same binary's digest, which is what keeps
// these cases exercising the command rather than the refusal a bw that is not
// the pin would get.
func stageFakeBW(t *testing.T, fields map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	items := filepath.Join(dir, "items.json")

	writePullFile(t, items, encodeItems(t, fields))
	writePullFile(t, filepath.Join(dir, bwFakeSettingsFile), encodeJSON(t, bwFakeSettings{ItemsFile: items}))

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}

	if linkErr := os.Symlink(self, filepath.Join(dir, bwFakeName)); linkErr != nil {
		t.Fatalf("link the fake bw: %v", linkErr)
	}

	return dir
}

// stageDigestRecord writes the record an earlier install would have left beside
// the binary it wrote, which is what separates a bw this tool owns from one it
// must not replace.
func stageDigestRecord(t *testing.T, binary, digest string) {
	t.Helper()

	writePullFile(t, binary+bwDigestRecord, digest+"\n")
}

// encodeItems renders the list response through encoding/json. Building it by
// concatenation would corrupt any value holding a quote, a backslash or a
// newline, which are exactly the values the escaping cases are about.
func encodeItems(t *testing.T, fields map[string]string) string {
	t.Helper()

	entries := make([]map[string]string, 0, len(fields))
	for name, value := range fields {
		entries = append(entries, map[string]string{"name": name, "value": value})
	}

	payload := map[string]any{
		"success": true,
		"data": map[string]any{
			"data": []map[string]any{{"name": pullItem, "fields": entries}},
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

// pathWith puts dir ahead of the real PATH.
func pathWith(dir string) string {
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// TestSecretsPullWithoutABlockIsANoOp covers FR3, the case every host without a
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

// TestSecretsPullRefusesAnUnpinnedBWElsewhereOnPATH covers FR8. A bw from a
// package manager or a hand install is not CairnOS's to replace, and a deploy
// that quietly drove it would be running the vault through a binary nothing in
// this repository vouches for.
func TestSecretsPullRefusesAnUnpinnedBWElsewhereOnPATH(t *testing.T) {
	binDir := stageFakeBW(t, bothFields())
	dir := pullConfig(t)

	t.Setenv("PATH", pathWith(binDir))

	code, _, stderr := runPinnedCLI(t, strangerDigest, secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want failure\nstderr: %s", code, stderr)
	}

	// Both paths, because the fix is a PATH change as often as an install.
	for _, want := range []string{filepath.Join(binDir, bwFakeName), ".local/bin"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("the refusal does not name %q:\n%s", want, stderr)
		}
	}
}

// TestSecretsPullUpgradesTheBWItInstalled is the other half of FR8: the binary
// the pin moved past sits in the install directory and carries the digest an
// earlier install recorded, so the command installs over it rather than asking
// for --force. The staged pin points at an asset nothing serves, so the case
// proves which branch ran by where it failed.
func TestSecretsPullUpgradesTheBWItInstalled(t *testing.T) {
	binDir := stageFakeBW(t, bothFields())
	dir := pullConfig(t)

	stageDigestRecord(t, filepath.Join(binDir, bwFakeName), testBinaryDigest(t))

	t.Setenv("PATH", pathWith(binDir))

	code, _, stderr := runPinnedCLI(t, strangerDigest,
		secretsCmd, "pull", flagDirArg, dir, "--bin-dir", binDir)
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want the unreachable download to fail\nstderr: %s", code, stderr)
	}

	if strings.Contains(stderr, "not the pinned release") {
		t.Fatalf("a bw in the install directory was refused instead of upgraded:\n%s", stderr)
	}

	if !strings.Contains(stderr, unreachableAsset) {
		t.Fatalf("the failure does not come from the install this should have started:\n%s", stderr)
	}
}

// TestSecretsPullUsesAnUnpinnedPlatformsOwnBW covers the platforms outside the
// two-entry pin table. There is no digest to compare against there, and before
// the pin check existed a working bw on PATH was simply used. Refusing the
// command instead would leave the operator no flag to get past it.
func TestSecretsPullUsesAnUnpinnedPlatformsOwnBW(t *testing.T) {
	binDir := stageFakeBW(t, bothFields())
	dir := pullConfig(t)

	t.Setenv("PATH", pathWith(binDir))

	code, stdout, stderr := runUnpinnedCLI(t, secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	// Used, but the operator is told it went unchecked.
	if !strings.Contains(stderr, "without checking it") {
		t.Fatalf("stderr does not say the bw went unchecked:\n%s", stderr)
	}

	if !strings.Contains(stdout, pullVarOne) {
		t.Fatalf("the pull did not render the vault fields:\n%s", stdout)
	}
}

func TestSecretsPullWritesTheEnvFile(t *testing.T) {
	binDir := stageFakeBW(t, bothFields())
	dir := pullConfig(t)

	t.Setenv("PATH", pathWith(binDir))

	code, stdout, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, dir)
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

	// The names belong in a deploy log; the values never do.
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
	binDir := stageFakeBW(t, bothFields())
	dir := pullConfig(t)

	t.Setenv("PATH", pathWith(binDir))

	path := filepath.Join(dir, "secrets.env")
	first := runPullAndRead(t, dir, path)

	if second := runPullAndRead(t, dir, path); second != first {
		t.Fatalf("two pulls differ:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// runPullAndRead runs one successful pull and returns the rendered file.
func runPullAndRead(t *testing.T, dir, path string) string {
	t.Helper()

	code, _, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	body, err := os.ReadFile(path) // #nosec G304 -- the path is built by the test
	if err != nil {
		t.Fatalf("read the rendered file: %v", err)
	}

	return string(body)
}

// TestSecretsPullDryRunWritesNothing covers FR8: an operator checks the mapping
// against the vault before a deploy touches the file the service reads.
func TestSecretsPullDryRunWritesNothing(t *testing.T) {
	binDir := stageFakeBW(t, bothFields())
	dir := pullConfig(t)

	t.Setenv("PATH", pathWith(binDir))

	code, stdout, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, dir, "--dry-run")
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

// TestSecretsPullNamesEveryMissingField covers FR6 end to end: the run fails,
// names everything to add, and leaves no file behind for the service to read.
func TestSecretsPullNamesEveryMissingField(t *testing.T) {
	binDir := stageFakeBW(t, map[string]string{pullFieldOne: pullValueOne})
	dir := pullConfig(t)

	t.Setenv("PATH", pathWith(binDir))

	code, _, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, dir)
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
	binDir := stageFakeBW(t, bothFields())
	dir := pullConfig(t)

	t.Setenv("PATH", pathWith(binDir))

	if err := os.Mkdir(filepath.Join(dir, "secrets.env"), 0o750); err != nil {
		t.Fatalf("stage a directory where the file goes: %v", err)
	}

	if code, _, _ := runCLI(t, secretsCmd, "pull", flagDirArg, dir); code != cli.ExitFailure {
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
	hostile := map[string]string{
		pullFieldOne: "two words $(touch /tmp/pwned) `id` done",
		pullFieldTwo: "it's got a quote and a \\backslash",
	}

	binDir := stageFakeBW(t, hostile)
	dir := pullConfig(t)

	t.Setenv("PATH", pathWith(binDir))

	if code, _, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, dir); code != cli.ExitOK {
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
	binDir := stageFakeBW(t, map[string]string{
		pullFieldOne: "first line\nsecond line",
		pullFieldTwo: pullValueTwo,
	})
	dir := pullConfig(t)

	t.Setenv("PATH", pathWith(binDir))

	code, _, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, dir)
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
//
//nolint:paralleltest // t.Setenv rewrites PATH for the whole process
func TestSecretsPullTargetsWhereTheUnitReads(t *testing.T) {
	binDir := stageFakeBW(t, bothFields())
	dir := pullConfigWithCredentials(t)

	t.Setenv("PATH", pathWith(binDir))

	cases := []struct {
		name string
		want string
		args []string
	}{
		{name: "user mode writes into the home", want: filepath.Join(dir, "secrets.env")},
		{name: "system mode writes under /etc", want: "/etc/cairnos/secrets.env", args: []string{"--system"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			argv := append([]string{secretsCmd, "pull", flagDirArg, dir, "--dry-run"}, testCase.args...)

			code, stdout, stderr := runCLI(t, argv...)
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

// TestSecretsPullInfersFieldsNamedLikeVariables covers FR19: a field whose name
// is already the variable name needs no mapping, which is what keeps a host's
// config from listing the same string twice. A field named for a human is left
// alone, because nothing says what variable it would fill.
func TestSecretsPullInfersFieldsNamedLikeVariables(t *testing.T) {
	binDir := stageFakeBW(t, map[string]string{
		inferredVar: inferredValue,
		humanField:  humanValue,
	})

	dir := writeBitwardenConfig(t, "[secrets.bitwarden]\nitem = \""+pullItem+"\"\n")

	t.Setenv("PATH", pathWith(binDir))

	code, stdout, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, dir)
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

// TestSecretsPullPrefersTheMappedField covers the collision half of FR19: an
// operator who wrote a mapping meant it, so the table beats the field that
// happens to share the variable's name.
func TestSecretsPullPrefersTheMappedField(t *testing.T) {
	binDir := stageFakeBW(t, map[string]string{
		inferredVar: inferredValue,
		humanField:  humanValue,
	})

	dir := writeBitwardenConfig(t, "[secrets.bitwarden]\nitem = \""+pullItem+"\"\n\n"+
		"[secrets.bitwarden.fields]\n"+inferredVar+" = \""+humanField+"\"\n")

	t.Setenv("PATH", pathWith(binDir))

	if code, _, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, dir); code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	rendered := runPullRead(t, filepath.Join(dir, "secrets.env"))

	if !strings.Contains(rendered, inferredVar+"='"+humanValue+"'") {
		t.Fatalf("the mapping did not win the collision:\n%s", rendered)
	}
}

// TestSecretsPullFailsWhenTheItemCarriesNothing covers the other FR19 edge: an
// item with no variable-named field and no table is a configuration the pull
// cannot act on, and the message has to name the item that was read.
func TestSecretsPullFailsWhenTheItemCarriesNothing(t *testing.T) {
	binDir := stageFakeBW(t, map[string]string{humanField: humanValue})

	dir := writeBitwardenConfig(t, "[secrets.bitwarden]\nitem = \""+pullItem+"\"\n")

	t.Setenv("PATH", pathWith(binDir))

	code, _, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want failure", code)
	}

	if !strings.Contains(stderr, pullItem) {
		t.Fatalf("stderr does not name the item that carried nothing:\n%s", stderr)
	}
}

// TestSecretsPullTakesTheItemFromTheCredentialsFile covers FR18: a host that
// ran `secrets auth` and never edited config.toml still pulls.
func TestSecretsPullTakesTheItemFromTheCredentialsFile(t *testing.T) {
	binDir := stageFakeBW(t, map[string]string{inferredVar: inferredValue})

	dir := t.TempDir()
	writePullFile(t, filepath.Join(dir, "config.toml"), "[secrets.bitwarden]\n")
	writePullFile(t, filepath.Join(dir, "bitwarden.env"),
		bitwarden.EnvItem+"="+pullItem+"\n"+credentialsFixture())

	t.Setenv("PATH", pathWith(binDir))

	if code, _, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, dir); code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	rendered := runPullRead(t, filepath.Join(dir, "secrets.env"))
	if !strings.Contains(rendered, inferredVar) {
		t.Fatalf("the pull did not use the item from the credentials file:\n%s", rendered)
	}
}

// TestSecretsPullWarnsAboutImpliedVariables covers FR20. The host will want a
// bot token and an API key at run time; the item carries neither. The pull says
// so and still writes, because a value may well be in the hand-written env file
// beside the generated one.
func TestSecretsPullWarnsAboutImpliedVariables(t *testing.T) {
	binDir := stageFakeBW(t, map[string]string{inferredVar: inferredValue})

	body := "[secrets.bitwarden]\nitem = \"" + pullItem + "\"\n\n" +
		"[bridge.mattermost]\nurl = \"https://mm.example.com\"\n\n" +
		"[bridge.mattermost.identity.ops]\nusername = \"ops-agent\"\nworkspace = \"default\"\n\n" +
		"[runner.openai]\nmodel = \"gpt-4.1\"\nbase_url = \"https://api.example.com/v1\"\n"

	dir := writeBitwardenConfig(t, body)

	t.Setenv("PATH", pathWith(binDir))

	code, _, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	// The shared default is what an operator should add, so that is the name
	// the warning carries; the per-identity override is mentioned separately.
	for _, want := range []string{"CAIRNOS_BRIDGE_MATTERMOST_TOKEN", "CAIRNOS_RUNNER_OPENAI_API_KEY"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("the warning does not name %s:\n%s", want, stderr)
		}
	}

	if !strings.Contains(stderr, "CAIRNOS_BRIDGE_MATTERMOST_<ID>_TOKEN") {
		t.Fatalf("the warning does not mention the per-identity override:\n%s", stderr)
	}
}

// TestSecretsPullWarnsWithTheRunnerBlockName covers the runner half of FR20
// under a block name of its own. The variable named is the one that runner
// reads first, which is what an operator has to add to the item.
func TestSecretsPullWarnsWithTheRunnerBlockName(t *testing.T) {
	binDir := stageFakeBW(t, map[string]string{inferredVar: inferredValue})

	body := "[secrets.bitwarden]\nitem = \"" + pullItem + "\"\n\n" +
		"[runner.zai]\ntype = \"openai\"\nmodel = \"glm\"\nbase_url = \"https://api.z.ai/v1\"\n"

	dir := writeBitwardenConfig(t, body)

	t.Setenv("PATH", pathWith(binDir))

	code, _, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, dir)
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
	binDir := stageFakeBW(t, map[string]string{"CAIRNOS_BRIDGE_MATTERMOST_OPS_TOKEN": "token-value"})

	body := "[secrets.bitwarden]\nitem = \"" + pullItem + "\"\n\n" +
		"[bridge.mattermost]\nurl = \"https://mm.example.com\"\n\n" +
		"[bridge.mattermost.identity.ops]\nusername = \"ops-agent\"\nworkspace = \"default\"\n"

	dir := writeBitwardenConfig(t, body)

	t.Setenv("PATH", pathWith(binDir))

	code, _, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if strings.Contains(stderr, "CAIRNOS_BRIDGE_MATTERMOST_TOKEN") {
		t.Fatalf("the pull warned about a token the item already carries:\n%s", stderr)
	}
}

// TestSecretsPullNamesAnIdentityTokenEnv covers the explicit key: an identity
// that names its own variable is warned about under that name, not the default.
func TestSecretsPullNamesAnIdentityTokenEnv(t *testing.T) {
	binDir := stageFakeBW(t, map[string]string{inferredVar: inferredValue})

	body := "[secrets.bitwarden]\nitem = \"" + pullItem + "\"\n\n" +
		"[bridge.mattermost]\nurl = \"https://mm.example.com\"\n\n" +
		"[bridge.mattermost.identity.ops]\nusername = \"ops-agent\"\nworkspace = \"default\"\n" +
		"token_env = \"CAIRNOS_SECOND_BOT_TOKEN\"\n"

	dir := writeBitwardenConfig(t, body)

	t.Setenv("PATH", pathWith(binDir))

	code, _, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stderr, "CAIRNOS_SECOND_BOT_TOKEN") {
		t.Fatalf("the warning does not name the identity's own variable:\n%s", stderr)
	}

	// Nothing falls back from an explicit name, so the override sentence would
	// be telling the operator about a name this config never reads.
	if strings.Contains(stderr, "CAIRNOS_BRIDGE_MATTERMOST_<ID>_TOKEN") {
		t.Fatalf("the warning offered an override an explicit token_env rules out:\n%s", stderr)
	}
}

// TestSecretsPullIsQuietAboutALocalRunner is the other side of FR20: a runner
// pointed at a server on this side of the network reads no key, so warning about
// one would train an operator to ignore the warnings that matter.
func TestSecretsPullIsQuietAboutALocalRunner(t *testing.T) {
	binDir := stageFakeBW(t, map[string]string{inferredVar: inferredValue})

	body := "[secrets.bitwarden]\nitem = \"" + pullItem + "\"\n\n" +
		"[runner.local]\ntype = \"openai\"\nmodel = \"gemma\"\nbase_url = \"http://127.0.0.1:8089/v1\"\n"

	dir := writeBitwardenConfig(t, body)

	t.Setenv("PATH", pathWith(binDir))

	code, _, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, dir)
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
// its half is pinned as exact bytes with the parse spelled out per case.
func TestSecretsPullRendersTheBytesSystemdParses(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		// systemd takes a single-quoted run literally, so the plain cases are
		// the value back with the wrap removed.
		{name: "a plain value", value: "abc", want: `CAIRNOS_ONE='abc'`},
		{name: "a value with spaces", value: "two words", want: `CAIRNOS_ONE='two words'`},
		// Outside quotes a backslash escapes; inside single quotes it does not,
		// so this reaches the service as typed.
		{name: "a value with a backslash", value: `a\b`, want: `CAIRNOS_ONE='a\b'`},
		// systemd performs no command substitution, and the quotes keep a
		// sourcing shell from performing any either.
		{name: "a value that looks like a command", value: "$(id)", want: `CAIRNOS_ONE='$(id)'`},
		{name: "a value with double quotes", value: `say "hi"`, want: `CAIRNOS_ONE='say "hi"'`},
		// The close, escaped quote, reopen sequence: both readers see one
		// literal quote, because the backslash sits outside the quoted runs.
		{name: "a value with a single quote", value: "it's", want: `CAIRNOS_ONE='it'\''s'`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			binDir := stageFakeBW(t, map[string]string{cairnosOneVar: testCase.value})
			dir := writeBitwardenConfig(t, "[secrets.bitwarden]\nitem = \""+pullItem+"\"\n")

			t.Setenv("PATH", pathWith(binDir))

			if code, _, stderr := runCLI(t, secretsCmd, "pull", flagDirArg, dir); code != cli.ExitOK {
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
// 0, so a deploy reports success having written nothing.
func TestSecretsPullSystemModeReadsTheSystemConfig(t *testing.T) {
	if _, err := os.Stat(systemConfigPath); err == nil {
		t.Skipf("%s exists on this host, so its contents would decide the case", systemConfigPath)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)

	// The user home does hold a block, so the case fails loudly if --system
	// ever falls back to it: the run would find the block and then fail on the
	// missing credentials instead of reporting the system path.
	userDir := filepath.Join(home, ".cairnos")
	if err := os.MkdirAll(userDir, 0o750); err != nil {
		t.Fatalf("stage the user home: %v", err)
	}

	writePullFile(t, filepath.Join(userDir, "config.toml"),
		"[secrets.bitwarden]\nitem = \""+pullItem+"\"\n")

	code, stdout, stderr := runCLI(t, secretsCmd, "pull", "--system")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stdout, systemConfigPath) {
		t.Fatalf("the run did not read %s:\n%s", systemConfigPath, stdout)
	}
}

// systemConfigPath is where a system install keeps its config, which is what the
// documented `sudo cairnos secrets pull --system` has to read.
const systemConfigPath = "/var/lib/cairnos/config.toml"
