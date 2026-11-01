package bitwarden_test

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/chadit/CairnOS/internal/bitwarden"
)

// Fixture values are deliberately short. The repository's secret scanner reads a
// long literal after one of these names as a hardcoded credential, and a test
// fixture is not worth training anyone to ignore that warning.
const (
	fixtureID     = "id-1"
	fixtureSecret = "sec-1"
	fixtureMaster = "mk-1"
)

// completeFile is a credentials file carrying all three values. The names come
// from the exported constants rather than literals so no assignment that reads
// like a hardcoded credential appears in this file.
func completeFile() string {
	return bitwarden.EnvClientID + "=" + fixtureID + "\n" +
		bitwarden.EnvClientSecret + "=" + fixtureSecret + "\n" +
		bitwarden.EnvMasterKey + "=" + fixtureMaster + "\n"
}

// noEnv is the empty environment, for cases where the file is the only source.
func noEnv(string) string { return "" }

// writeCredentials writes a credentials file at the given mode and returns its
// path.
func writeCredentials(t *testing.T, content string, perm os.FileMode) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "bitwarden.env")
	writeFixture(t, path, content, perm)

	return path
}

func TestLoadCredentialsReadsTheFile(t *testing.T) {
	t.Parallel()

	// Comments, an export prefix, both quote styles, and an unrelated key are
	// what an operator copying from a shell file actually writes.
	path := writeCredentials(t, strings.Join([]string{
		"# the API key for this host",
		bitwarden.EnvClientID + "=" + fixtureID,
		bitwarden.EnvClientSecret + `="` + fixtureSecret + `"`,
		"export " + bitwarden.EnvMasterKey + "='" + fixtureMaster + "'",
		"UNRELATED=ignored",
		"",
	}, "\n"), 0o600)

	creds, err := bitwarden.LoadCredentials(path, noEnv)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if creds.ClientID != fixtureID || creds.ClientSecret != fixtureSecret {
		t.Fatalf("creds = %+v, want both API values unquoted", creds)
	}

	if creds.MasterKey != fixtureMaster {
		t.Fatalf("master key = %q, want the single-quoted value unwrapped", creds.MasterKey)
	}
}

// TestLoadCredentialsRefusesAReadableFile is the one check worth failing a
// deploy over: the file opens a whole vault, so a warning nobody reads would be
// the same as no check.
func TestLoadCredentialsRefusesAReadableFile(t *testing.T) {
	t.Parallel()

	for _, perm := range []os.FileMode{0o644, 0o640, 0o604, 0o666} {
		path := writeCredentials(t, completeFile(), perm)

		_, err := bitwarden.LoadCredentials(path, noEnv)
		if err == nil {
			t.Fatalf("mode %04o was accepted; it must be 0600 or stricter", perm)
		}

		message := err.Error()
		if !strings.Contains(message, "0600") {
			t.Fatalf("error %q does not say what the mode has to be", message)
		}
	}
}

// TestLoadCredentialsAcceptsStricterModes keeps the check a ceiling rather than
// an equality, so an operator who went further is not refused.
func TestLoadCredentialsAcceptsStricterModes(t *testing.T) {
	t.Parallel()

	for _, perm := range []os.FileMode{0o600, 0o400} {
		path := writeCredentials(t, completeFile(), perm)

		if _, err := bitwarden.LoadCredentials(path, noEnv); err != nil {
			t.Fatalf("mode %04o was refused: %v", perm, err)
		}
	}
}

// TestLoadCredentialsFallsBackToTheEnvironment lets a host keep some values in
// the file and the rest wherever it already keeps secrets.
func TestLoadCredentialsFallsBackToTheEnvironment(t *testing.T) {
	t.Parallel()

	path := writeCredentials(t, bitwarden.EnvClientID+"="+fixtureID+"\n", 0o600)

	const envSecret = "env-sec"

	env := map[string]string{
		bitwarden.EnvClientSecret: envSecret,
		bitwarden.EnvMasterKey:    "env-mk",
	}

	creds, err := bitwarden.LoadCredentials(path, func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if creds.ClientID != fixtureID || creds.ClientSecret != envSecret {
		t.Fatalf("creds = %+v, want the file to win and the environment to fill in", creds)
	}
}

// TestLoadCredentialsWorksWithNoFileAtAll covers the host that keeps all three
// in its environment, where an absent file is the normal state.
func TestLoadCredentialsWorksWithNoFileAtAll(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		bitwarden.EnvClientID:     fixtureID,
		bitwarden.EnvClientSecret: fixtureSecret,
		bitwarden.EnvMasterKey:    fixtureMaster,
	}

	absent := filepath.Join(t.TempDir(), "not-there.env")

	creds, err := bitwarden.LoadCredentials(absent, func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if creds.ClientID != fixtureID || creds.MasterKey != fixtureMaster {
		t.Fatalf("creds = %+v, want all three from the environment", creds)
	}
}

// TestLoadCredentialsNamesEveryMissingValue tells the operator everything to fix
// in one run rather than one value per attempt.
func TestLoadCredentialsNamesEveryMissingValue(t *testing.T) {
	t.Parallel()

	path := writeCredentials(t, bitwarden.EnvClientID+"="+fixtureID+"\n", 0o600)

	_, err := bitwarden.LoadCredentials(path, noEnv)
	if err == nil {
		t.Fatal("load accepted a file missing two of the three values")
	}

	message := err.Error()
	for _, want := range []string{bitwarden.EnvClientSecret, bitwarden.EnvMasterKey} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q does not name %s", message, want)
		}
	}

	assertNoValues(t, message, fixtureID)
}

// assertNoValues fails when an error message carries any of the values a
// credentials file held, since that message lands in a deploy log.
func assertNoValues(t *testing.T, message string, values ...string) {
	t.Helper()

	for _, value := range values {
		if strings.Contains(message, value) {
			t.Fatalf("the error carries a value: %s", message)
		}
	}
}

// fixtureItem is the vault item a chezmoi config names alongside the three
// credentials.
const fixtureItem = "item-1"

// The [data.secret] keys a chezmoi config carries the four values under.
const (
	chezmoiClientID     = "bw_client_id"
	chezmoiClientSecret = "bw_client_secret"
	chezmoiMasterKey    = "bw_master_key"
	chezmoiItem         = "bw_item_name"
)

// chezmoiEntry is one key = value line of a chezmoi config's [data.secret] table.
type chezmoiEntry struct{ key, value string }

// chezmoiConfig renders a chezmoi config whose [data.secret] table carries the
// given keys, between the unrelated sections a real one holds, so every case
// also proves those sections are skipped.
func chezmoiConfig(entries ...chezmoiEntry) string {
	var out strings.Builder

	out.WriteString("[data]\nemail = \"ops@example.com\"\n\n[data.secret]\n")

	for _, entry := range entries {
		fmt.Fprintf(&out, "%s = %q\n", entry.key, entry.value)
	}

	out.WriteString("\n[git]\nautoCommit = true\n")

	return out.String()
}

// TestLoadCredentialsReadsAChezmoiConfig covers a credentials_file ending in
// .toml, which is read as a chezmoi config so a workstation keeps one copy of
// the vault credentials. It gets the same mode check and environment fallback
// as the KEY=VALUE file.
func TestLoadCredentialsReadsAChezmoiConfig(t *testing.T) {
	t.Parallel()

	const envMaster = "env-mk"

	allFour := chezmoiConfig(
		chezmoiEntry{chezmoiClientID, fixtureID},
		chezmoiEntry{chezmoiClientSecret, fixtureSecret},
		chezmoiEntry{chezmoiMasterKey, fixtureMaster},
		chezmoiEntry{chezmoiItem, fixtureItem},
	)

	fixtureCreds := bitwarden.Credentials{
		ClientID: fixtureID, ClientSecret: fixtureSecret, MasterKey: fixtureMaster, Item: fixtureItem,
	}

	fullEnv := map[string]string{
		bitwarden.EnvClientID:     fixtureID,
		bitwarden.EnvClientSecret: fixtureSecret,
		bitwarden.EnvMasterKey:    fixtureMaster,
		bitwarden.EnvItem:         fixtureItem,
	}

	// idLine puts a valid line ahead of the master key's, so a decode error at
	// line 3 is one the master key line caused.
	idLine := "[data.secret]\n" + chezmoiClientID + " = \"" + fixtureID + "\"\n"

	const masterKeyLineErr = "cannot be read as a chezmoi config at line 3,"

	cases := []struct {
		env      map[string]string
		want     bitwarden.Credentials
		name     string
		fileName string
		content  string
		wantErr  string
		perm     os.FileMode
	}{
		{
			name:    "all four keys at mode 0600",
			content: allFour,
			perm:    0o600,
			want:    fixtureCreds,
		},
		{
			name: "no bw_master_key with the master key in the environment",
			content: chezmoiConfig(
				chezmoiEntry{chezmoiClientID, fixtureID},
				chezmoiEntry{chezmoiClientSecret, fixtureSecret},
				chezmoiEntry{chezmoiItem, fixtureItem},
			),
			perm: 0o600,
			env:  map[string]string{bitwarden.EnvMasterKey: envMaster},
			want: bitwarden.Credentials{
				ClientID: fixtureID, ClientSecret: fixtureSecret, MasterKey: envMaster, Item: fixtureItem,
			},
		},
		{
			name:     "all four keys in a file named chezmoi.TOML",
			fileName: "chezmoi.TOML",
			content:  allFour,
			perm:     0o600,
			want:     fixtureCreds,
		},
		{
			name: "empty bw_master_key with the master key in the environment",
			content: chezmoiConfig(
				chezmoiEntry{chezmoiClientID, fixtureID},
				chezmoiEntry{chezmoiClientSecret, fixtureSecret},
				chezmoiEntry{chezmoiMasterKey, ""},
				chezmoiEntry{chezmoiItem, fixtureItem},
			),
			perm: 0o600,
			env:  map[string]string{bitwarden.EnvMasterKey: envMaster},
			want: bitwarden.Credentials{
				ClientID: fixtureID, ClientSecret: fixtureSecret, MasterKey: envMaster, Item: fixtureItem,
			},
		},
		{
			name:    "no [data] table with every value in the environment",
			content: "[git]\nautoCommit = true\n",
			perm:    0o600,
			env:     fullEnv,
			want:    fixtureCreds,
		},
		{
			name:    "empty [data.secret] table with nothing in the environment",
			content: chezmoiConfig(),
			perm:    0o600,
			wantErr: "has no [data.secret] " + chezmoiClientID + ", " + chezmoiClientSecret + ", " +
				chezmoiMasterKey + ", and the environment has no " + bitwarden.EnvClientID + ", " +
				bitwarden.EnvClientSecret + ", " + bitwarden.EnvMasterKey,
		},
		{
			name:    "all four keys at mode 0644",
			content: allFour,
			perm:    0o644,
			wantErr: "0600",
		},
		{
			name:    "KEY=VALUE lines behind a .toml name",
			content: completeFile(),
			perm:    0o600,
			wantErr: "cannot be read as a chezmoi config at line 1,",
		},
		{
			name:    "unterminated string on the bw_master_key line",
			content: idLine + chezmoiMasterKey + " = \"" + fixtureMaster + "\n",
			perm:    0o600,
			wantErr: masterKeyLineErr,
		},
		{
			name:    "integer bw_master_key in otherwise valid TOML",
			content: idLine + chezmoiMasterKey + " = 12345\n",
			perm:    0o600,
			wantErr: masterKeyLineErr,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), cmp.Or(testCase.fileName, "chezmoi.toml"))
			writeFixture(t, path, testCase.content, testCase.perm)

			creds, err := bitwarden.LoadCredentials(path, func(name string) string { return testCase.env[name] })

			if testCase.wantErr != "" {
				assertCredentialsRefused(t, err, testCase.wantErr)

				return
			}

			if err != nil {
				t.Fatalf("load: %v", err)
			}

			if creds != testCase.want {
				t.Fatalf("creds = %+v, want %+v", creds, testCase.want)
			}
		})
	}
}

// assertCredentialsRefused checks a refusal says what to fix and carries none
// of the values it read, since a parse error on this file sits next to the
// master key. The decoder's error must not be wrapped either: its String()
// prints the line Error() omits, putting the master key one formatting verb
// from a log.
func assertCredentialsRefused(t *testing.T, err error, want string) {
	t.Helper()

	if err == nil {
		t.Fatalf("load succeeded, want an error naming %q", want)
	}

	message := err.Error()
	if !strings.Contains(message, want) {
		t.Fatalf("error %q does not contain %q", message, want)
	}

	assertNoValues(t, message, fixtureID, fixtureSecret, fixtureMaster)

	if _, wrapsDecodeErr := errors.AsType[*toml.DecodeError](err); wrapsDecodeErr {
		t.Fatalf("the error wraps the TOML decoder's error, which quotes the failing line: %s", message)
	}
}
