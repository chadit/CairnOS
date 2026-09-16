package bitwarden_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/bitwarden"
	"github.com/chadit/CairnOS/internal/cairnerr"
)

// The item and fields every case reads, spelled the way a host config would.
const (
	vaultItem = "chezmoi"

	// statusUnauthenticated is the one bw status value that triggers a login.
	statusUnauthenticated = "unauthenticated"

	fieldToken = "mattermost token"
	fieldKey   = "zai key"
	valueToken = "token-value"
	valueKey   = "key-value"
)

// testCredentials are the three values a case hands the client. None of them may
// reach argv, which is what the recorded invocations prove.
func testCredentials() bitwarden.Credentials {
	return bitwarden.Credentials{
		ClientID:     "client-id-value",
		ClientSecret: "client-secret-value",
		MasterKey:    "master-key-value",
	}
}

// oneItem is the happy-path fixture: the named item carrying both fields.
func oneItem() string {
	return itemsJSON(itemJSON(vaultItem, map[string]string{
		fieldToken: valueToken,
		fieldKey:   valueKey,
	}))
}

// newClient builds a client pointed at the staged fake, with a short timeout so
// a case that hangs fails rather than waiting out the default minute. The binary
// is the absolute symlink path, which is what LookPath hands the command in
// production and what lets the child find its own fixtures.
func newClient(fake fakeBW) *bitwarden.Client {
	return &bitwarden.Client{Binary: fake.binary, Timeout: 20 * time.Second}
}

// wantCode asserts the failure carries one canonical code.
func wantCode(t *testing.T, err error, code string) {
	t.Helper()

	classified, ok := errors.AsType[*cairnerr.Error](err)
	if !ok {
		t.Fatalf("error %v is not a cairnerr.Error", err)
	}

	if classified.Code != code {
		t.Fatalf("error code = %q, want %q (%v)", classified.Code, code, err)
	}
}

func TestFieldsReadsTheNamedFields(t *testing.T) {
	t.Parallel()

	fake := stageFakeBW(t, oneItem(), fakeSettings{})

	values, err := newClient(fake).Fields(t.Context(), testCredentials(), vaultItem,
		[]string{fieldToken, fieldKey})
	if err != nil {
		t.Fatalf("fields: %v", err)
	}

	if values[fieldToken] != valueToken || values[fieldKey] != valueKey {
		t.Fatalf("values = %+v, want both fields read off the item", values)
	}

	// The whole point of driving serve: unlock, sync and list all happen, and the
	// vault is locked again before the call returns.
	calls := recorded(t, fake.calls)
	for _, want := range []string{"/unlock", "/sync", "/list/object/items", "/lock"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("calls %q missing %s", calls, want)
		}
	}
}

// TestFieldsKeepsEveryValueOutOfArgv is the reason the API key rides in the
// child's environment and the master key in a request body: arguments are
// readable from /proc by anything the same user runs.
func TestFieldsKeepsEveryValueOutOfArgv(t *testing.T) {
	t.Parallel()

	fake := stageFakeBW(t, oneItem(), fakeSettings{Status: statusUnauthenticated})

	creds := testCredentials()

	if _, err := newClient(fake).Fields(t.Context(), creds, vaultItem, []string{fieldToken}); err != nil {
		t.Fatalf("fields: %v", err)
	}

	argv := recorded(t, fake.argv)
	for _, secret := range []string{creds.ClientID, creds.ClientSecret, creds.MasterKey} {
		if strings.Contains(argv, secret) {
			t.Fatalf("a credential reached argv:\n%s", argv)
		}
	}

	// The body record carries key names only, so this proves the master key went
	// where it was supposed to without printing it.
	if calls := recorded(t, fake.calls); !strings.Contains(calls, "body:password") {
		t.Fatalf("calls %q show no unlock body", calls)
	}
}

// TestFieldsLogsInOnlyWhenUnauthenticated covers both halves of the branch: bw
// stores credentials after the first login, so repeating it every deploy would
// be a wasted round trip against the API.
func TestFieldsLogsInOnlyWhenUnauthenticated(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		status    string
		wantLogin bool
	}{
		{name: "unauthenticated logs in", status: statusUnauthenticated, wantLogin: true},
		{name: "locked does not", status: "locked", wantLogin: false},
		{name: "unlocked does not", status: "unlocked", wantLogin: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			fake := stageFakeBW(t, oneItem(), fakeSettings{Status: testCase.status})

			if _, err := newClient(fake).Fields(t.Context(), testCredentials(), vaultItem,
				[]string{fieldToken}); err != nil {
				t.Fatalf("fields: %v", err)
			}

			gotLogin := strings.Contains(recorded(t, fake.argv), "login")
			if gotLogin != testCase.wantLogin {
				t.Fatalf("login ran = %v, want %v for status %q", gotLogin, testCase.wantLogin, testCase.status)
			}
		})
	}
}

func TestFieldsReportsALoginFailure(t *testing.T) {
	t.Parallel()

	fake := stageFakeBW(t, oneItem(), fakeSettings{Status: statusUnauthenticated, LoginFails: true})

	_, err := newClient(fake).Fields(t.Context(), testCredentials(), vaultItem, []string{fieldToken})
	wantCode(t, err, bitwarden.CodeLogin)

	message := err.Error()
	if strings.Contains(message, testCredentials().ClientSecret) {
		t.Fatalf("the login error carries a value: %s", message)
	}
}

// TestFieldsNamesEveryMissingField covers FR6: one run tells the operator
// everything to add to the item rather than one field per attempt.
func TestFieldsNamesEveryMissingField(t *testing.T) {
	t.Parallel()

	fake := stageFakeBW(t, itemsJSON(itemJSON(vaultItem, map[string]string{fieldToken: valueToken})), fakeSettings{})

	_, err := newClient(fake).Fields(t.Context(), testCredentials(), vaultItem,
		[]string{fieldToken, fieldKey, "another field"})
	wantCode(t, err, bitwarden.CodeFields)

	message := err.Error()
	for _, want := range []string{fieldKey, "another field"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q does not name the missing field %q", message, want)
		}
	}
}

// TestFieldsTreatsAnEmptyFieldAsMissing keeps a blank value out of the generated
// file, where it would read as configured and behave as if it were not.
func TestFieldsTreatsAnEmptyFieldAsMissing(t *testing.T) {
	t.Parallel()

	fake := stageFakeBW(t, itemsJSON(itemJSON(vaultItem, map[string]string{fieldToken: ""})), fakeSettings{})

	_, err := newClient(fake).Fields(t.Context(), testCredentials(), vaultItem, []string{fieldToken})
	wantCode(t, err, bitwarden.CodeFields)
}

// TestFieldsRefusesTwoItemsWithOneName is why the item is matched exactly: the
// endpoint is a substring search, so picking the first hit silently reads the
// wrong item whenever two names share a prefix.
func TestFieldsRefusesTwoItemsWithOneName(t *testing.T) {
	t.Parallel()

	duplicate := itemsJSON(
		itemJSON(vaultItem, map[string]string{fieldToken: valueToken}),
		itemJSON(vaultItem, map[string]string{fieldToken: "the other one"}),
	)

	fake := stageFakeBW(t, duplicate, fakeSettings{})

	_, err := newClient(fake).Fields(t.Context(), testCredentials(), vaultItem, []string{fieldToken})
	wantCode(t, err, bitwarden.CodeItem)
}

// TestFieldsIgnoresAPrefixMatch proves the exact-name rule on the shape that
// motivates it: a longer item whose name contains the configured one.
func TestFieldsIgnoresAPrefixMatch(t *testing.T) {
	t.Parallel()

	fake := stageFakeBW(t, itemsJSON(
		itemJSON(vaultItem+"-old", map[string]string{fieldToken: "stale"}),
		itemJSON(vaultItem, map[string]string{fieldToken: valueToken}),
	), fakeSettings{})

	values, err := newClient(fake).Fields(t.Context(), testCredentials(), vaultItem, []string{fieldToken})
	if err != nil {
		t.Fatalf("fields: %v", err)
	}

	if values[fieldToken] != valueToken {
		t.Fatalf("value = %q, want the exactly-named item's value", values[fieldToken])
	}
}

func TestFieldsReportsAnItemThatIsNotThere(t *testing.T) {
	t.Parallel()

	fake := stageFakeBW(t, itemsJSON(), fakeSettings{})

	_, err := newClient(fake).Fields(t.Context(), testCredentials(), vaultItem, []string{fieldToken})
	wantCode(t, err, bitwarden.CodeItem)
}

// TestFieldsGivesUpWhenServeNeverAnswers bounds the one step with no natural
// limit. The fake starts and stays alive without listening, which is what a
// daemon that fails to bind looks like from here.
func TestFieldsGivesUpWhenServeNeverAnswers(t *testing.T) {
	t.Parallel()

	fake := stageFakeBW(t, oneItem(), fakeSettings{NoServe: true})

	start := time.Now()

	_, err := newClient(fake).Fields(t.Context(), testCredentials(), vaultItem, []string{fieldToken})
	wantCode(t, err, bitwarden.CodeReady)

	// The readiness budget is 15s; anything near the fake's 30s sleep would mean
	// the call waited on the process instead of its own deadline.
	if elapsed := time.Since(start); elapsed > 25*time.Second {
		t.Fatalf("the call took %s, well past the readiness budget", elapsed)
	}
}

// TestFieldsLocksAndStopsTheDaemonOnFailure is the cleanup guarantee: an
// interrupted pull must not leave an unlocked vault answering on loopback. It
// checks both halves, because locking without stopping still leaks the process
// and the port, and the lock record alone would pass with the cancel removed.
func TestFieldsLocksAndStopsTheDaemonOnFailure(t *testing.T) {
	t.Parallel()

	fake := stageFakeBW(t, itemsJSON(), fakeSettings{})

	if _, err := newClient(fake).Fields(t.Context(), testCredentials(), vaultItem,
		[]string{fieldToken}); err == nil {
		t.Fatal("fields returned nil for an item that is not in the vault")
	}

	if calls := recorded(t, fake.calls); !strings.Contains(calls, "/lock") {
		t.Fatalf("calls %q show no lock after a failure", calls)
	}

	port := servePort(t, recorded(t, fake.argv))
	if listening(t.Context(), port) {
		t.Fatalf("the daemon still answers on port %s after the call returned", port)
	}
}
