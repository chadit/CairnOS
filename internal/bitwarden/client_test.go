package bitwarden_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/bitwarden"
	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/environ"
)

// The item and fields every case reads, spelled the way a host config would.
const (
	vaultItem = "chezmoi"

	// statusUnauthenticated is the one bw status value that triggers a login.
	statusUnauthenticated = "unauthenticated"

	// statusUnlocked is what the fake reports when a case sets no status.
	statusUnlocked = "unlocked"

	fieldToken   = "mattermost token"
	fieldKey     = "zai key"
	fieldMissing = "another field"
	valueToken   = "token-value"
	valueKey     = "key-value"
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

// readyPolls is how many polls the readiness budget lasts in these cases. A
// child normally binds within two, so this is ample room on a busy host, and it
// is finite: a daemon that answers neither way ends the wait instead of hanging
// the run. Nothing longer helps, because a host that drops loopback traffic
// keeps dropping it for minutes.
const readyPolls = 120

// newClient builds a client pointed at the staged fake. The binary is the
// absolute symlink path, which lets the child find its own fixtures.
//
// The stepping clock keeps these cases off a stopwatch: the budget is spent in
// polls, so the wait ends when the fake's listener answers rather than when a
// loaded host runs past a wall-clock deadline.
func newClient(fake fakeBW) *bitwarden.Client {
	return &bitwarden.Client{
		Binary:  fake.binary,
		Timeout: 20 * time.Second,
		Env:     environ.Map(map[string]string{"HOME": filepath.Dir(fake.binary)}),
		Clock:   bitwarden.NewSteppingClock(readyPolls),
	}
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
		{name: "unlocked does not", status: statusUnlocked, wantLogin: false},
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
		[]string{fieldToken, fieldKey, fieldMissing})
	wantCode(t, err, bitwarden.CodeFields)

	message := err.Error()
	for _, want := range []string{fieldKey, fieldMissing} {
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

// A list that is not JSON keeps the decode error in the chain, so a caller can
// tell a garbled answer from a vault that has no such item.
func TestFieldsKeepsTheDecodeErrorForAGarbledList(t *testing.T) {
	t.Parallel()

	fake := stageFakeBW(t, "not json", fakeSettings{})

	_, err := newClient(fake).Fields(t.Context(), testCredentials(), vaultItem, []string{fieldToken})
	wantCode(t, err, bitwarden.CodeList)

	if _, isSyntax := errors.AsType[*json.SyntaxError](err); !isSyntax {
		t.Fatalf("errors.As found no *json.SyntaxError behind %v", err)
	}
}

// TestFieldsGivesUpWhenServeNeverAnswers bounds the one step with no natural
// limit. The fake stays alive on its port and answers nothing, so no amount of
// waiting gets a reply. A one-poll budget ends each wait on the first miss, so
// the case proves the whole thing ends without the suite sitting through it.
func TestFieldsGivesUpWhenServeNeverAnswers(t *testing.T) {
	t.Parallel()

	fake := stageFakeBW(t, oneItem(), fakeSettings{SilentServes: startAttempts + 1})

	client := newClient(fake)
	client.Clock = bitwarden.NewSteppingClock(1)

	_, err := client.Fields(t.Context(), testCredentials(), vaultItem, []string{fieldToken})
	wantCode(t, err, bitwarden.CodeReady)

	// Starts are counted from the failure, not the fake's argv record: the
	// one-poll wait kills each child about 250ms after it starts, and a child
	// slower than that on a loaded host dies before it records itself. The text
	// is also what the operator reads, so every start names its own quiet port.
	message := err.Error()
	quiet := regexp.MustCompile(`never answered on port (\d+)`).FindAllStringSubmatch(message, -1)

	ports := make(map[string]bool, len(quiet))
	for _, match := range quiet {
		ports[match[1]] = true
	}

	if len(quiet) != startAttempts || len(ports) != startAttempts {
		t.Fatalf("the failure names %d quiet starts on %d ports, want %d on distinct ports: %s",
			len(quiet), len(ports), startAttempts, message)
	}
}

// silentPolls is the budget for the case that has to spend one. It is small
// enough that the first wait gives up in seconds and still leaves the second
// start far more room than a child needs to bind, and a start lost to a slow
// host has the third to fall back on.
const silentPolls = 20

// TestFieldsMovesOnFromAPortThatGoesQuiet covers the other unusable port: the
// child is up on its port and answers nothing, which is how a port the host
// stopped carrying traffic for looks from here. Waiting longer on it never
// helped, so the budget running out has to start over somewhere else.
func TestFieldsMovesOnFromAPortThatGoesQuiet(t *testing.T) {
	t.Parallel()

	fake := stageFakeBW(t, oneItem(), fakeSettings{SilentServes: 1})

	client := newClient(fake)
	client.Clock = bitwarden.NewSteppingClock(silentPolls)

	values, err := client.Fields(t.Context(), testCredentials(), vaultItem, []string{fieldToken})
	if err != nil {
		t.Fatalf("fields: %v", err)
	}

	if values[fieldToken] != valueToken {
		t.Fatalf("value = %q, want the item read over the second daemon", values[fieldToken])
	}

	ports := servePorts(recorded(t, fake.argv))
	if len(ports) != 2 {
		t.Fatalf("serve ran %d times, want one that went quiet and one that answered: %v", len(ports), ports)
	}
}

// TestFieldsMovesToAnotherPortWhenTheChildLosesIt covers the race the protocol
// cannot design away: the port is released before the child binds it, so
// anything else on the host can take it in between. The fake binds a port this
// case holds open, which fails the way that lost port does.
func TestFieldsMovesToAnotherPortWhenTheChildLosesIt(t *testing.T) {
	t.Parallel()

	fake := stageFakeBW(t, oneItem(), fakeSettings{BusyPort: occupiedPort(t), BusyServes: 1})

	values, err := newClient(fake).Fields(t.Context(), testCredentials(), vaultItem, []string{fieldToken})
	if err != nil {
		t.Fatalf("fields: %v", err)
	}

	if values[fieldToken] != valueToken {
		t.Fatalf("value = %q, want the item read over the second daemon", values[fieldToken])
	}

	ports := servePorts(recorded(t, fake.argv))
	if len(ports) != 2 {
		t.Fatalf("serve ran %d times, want one that lost its port and one that answered: %v", len(ports), ports)
	}

	if ports[0] == ports[1] {
		t.Fatalf("the second start reused port %s, which is the port that was taken", ports[0])
	}
}

// startAttempts is how many ports one read will try before reporting. A host
// where every start loses its port has to say so rather than keep spawning
// children a deploy is waiting on.
const startAttempts = 3

// TestFieldsGivesUpAfterEveryPortIsLost pins that bound and keeps the child's
// own words and exit status in the failure: without them the operator sees a
// daemon that went quiet, not a port that was already taken.
func TestFieldsGivesUpAfterEveryPortIsLost(t *testing.T) {
	t.Parallel()

	busy := occupiedPort(t)
	fake := stageFakeBW(t, oneItem(), fakeSettings{BusyPort: busy, BusyServes: startAttempts + 1})

	_, err := newClient(fake).Fields(t.Context(), testCredentials(), vaultItem, []string{fieldToken})
	wantCode(t, err, bitwarden.CodeReady)

	if got := len(servePorts(recorded(t, fake.argv))); got != startAttempts {
		t.Fatalf("serve ran %d times, want %d", got, startAttempts)
	}

	message := err.Error()
	for _, want := range []string{"fake serve", strconv.Itoa(busy), "exit status " + strconv.Itoa(exitStatusFailed)} {
		if !strings.Contains(message, want) {
			t.Fatalf("the failure does not carry how the child exited (%q missing): %s", want, message)
		}
	}
}

// TestFieldsSurvivesAProbeThatGoesUnanswered covers the loopback path this host
// can drop on a recycled port: one probe gets nothing back and costs its own
// short bound, and the next one finds the same daemon listening. Restarting
// there would throw away a daemon that was up.
func TestFieldsSurvivesAProbeThatGoesUnanswered(t *testing.T) {
	t.Parallel()

	fake := stageFakeBW(t, oneItem(), fakeSettings{StallFirstProbe: true})

	values, err := newClient(fake).Fields(t.Context(), testCredentials(), vaultItem, []string{fieldToken})
	if err != nil {
		t.Fatalf("fields: %v", err)
	}

	if values[fieldToken] != valueToken {
		t.Fatalf("value = %q, want the item read after the stalled probe", values[fieldToken])
	}

	if got := len(servePorts(recorded(t, fake.argv))); got != 1 {
		t.Fatalf("serve ran %d times, want the one daemon that stayed up", got)
	}
}

// TestFieldsLocksAndStopsTheDaemonOnFailure is the cleanup guarantee: an
// interrupted pull must not leave an unlocked vault answering on loopback. It
// checks both halves, because locking without stopping still leaks the process
// and the port, and the lock record alone would pass with the cancel removed.
func TestFieldsLocksAndStopsTheDaemonOnFailure(t *testing.T) {
	t.Parallel()

	fake := stageFakeBW(t, itemsJSON(), fakeSettings{})

	// The code, not just any error: a readiness failure would also be non-nil and
	// would leave the lock record empty for a reason that has nothing to do with
	// the cleanup this case is about.
	_, err := newClient(fake).Fields(t.Context(), testCredentials(), vaultItem, []string{fieldToken})
	wantCode(t, err, bitwarden.CodeItem)

	if calls := recorded(t, fake.calls); !strings.Contains(calls, "/lock") {
		t.Fatalf("calls %q show no lock after a failure", calls)
	}

	port := servePort(t, recorded(t, fake.argv))
	if listening(t.Context(), port) {
		t.Fatalf("the daemon still answers on port %s after the call returned", port)
	}
}
