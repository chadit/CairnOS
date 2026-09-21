package bitwarden_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeName is the symlink name for this test binary and the dispatch signal: a
// process whose argv[0] ends in it is a child the client launched. An
// environment marker would not work, because the client scrubs its child's
// environment to an allowlist and the fake would re-run the suite instead.
const fakeName = "bw"

// fakeSettingsFile sits beside the symlink and carries everything the fake needs,
// for the same reason: a file survives the environment scrub.
const fakeSettingsFile = "fake.json"

// Modes the fixtures write.
const (
	fixtureExecPerm = 0o700
	fixtureFilePerm = 0o600
)

// headerTimeout bounds how long the fake daemon waits for a request header,
// so a stalled test cannot leave the child holding its port forever.
const headerTimeout = 30 * time.Second

// The three fixture files, all beside the symlink under fixed names that both
// sides derive, so the settings carry no path the fake could resolve for itself.
// items.json is the list endpoint's body; the other two are the records a case
// reads back.
const (
	fakeItemsFileName = "items.json"
	fakeArgvFileName  = "argv.txt"
	fakeCallsFileName = "calls.txt"
)

// fakeSettings is the fake's whole configuration, written next to the symlink.
type fakeSettings struct {
	// Status is what `bw status` reports; empty means unlocked.
	Status string `json:"status"`

	// BusyPort is a port the case holds open, and BusyServes how many serve
	// invocations bind it instead of the port they were given. That is the shape
	// of a child whose port went to something else before it got there.
	BusyPort   int `json:"busy_port"`
	BusyServes int `json:"busy_serves"`

	// SilentServes is how many serve invocations stay up without ever binding or
	// answering, the shape of a daemon whose port stopped carrying traffic.
	SilentServes int `json:"silent_serves"`

	// LoginFails makes `bw login` exit non-zero.
	LoginFails bool `json:"login_fails"`

	// StallFirstProbe leaves the first request unanswered until the caller gives
	// up, the shape of a loopback dial this host drops.
	StallFirstProbe bool `json:"stall_first_probe"`
}

// fakeDirFile resolves one fixture beside the symlink this process was launched
// through, which is the only directory the fake can find on its own.
func fakeDirFile(name string) string {
	return filepath.Join(filepath.Dir(os.Args[0]), name)
}

// IsFakeInvocation reports whether this process was launched as the fake CLI.
func isFakeInvocation() bool {
	return filepath.Base(os.Args[0]) == fakeName
}

// runFakeBW is the fake `bw`, reached by re-executing this test binary through a
// symlink named bw. Keeping the fake in Go means it compiles and type-checks with
// the rest of the package and depends on no interpreter, so nothing skips on a
// host that happens to lack one. It returns the process exit code.
func runFakeBW(argv []string) int {
	if len(argv) == 0 {
		return 2
	}

	// serve runs its own settings read, because what it does with them decides
	// whether it ever binds.
	if argv[0] == "serve" {
		return runFakeServe(argv)
	}

	settings, err := loadFakeSettings()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake bw: %v\n", err)

		return 3
	}

	appendLine(fakeDirFile(fakeArgvFileName), strings.Join(argv, " "))

	switch argv[0] {
	case "status":
		return reportStatus(settings)
	case "login":
		return reportLogin(settings)
	default:
		fmt.Fprintf(os.Stderr, "unexpected subcommand: %s\n", argv[0])

		return 2
	}
}

// loadFakeSettings reads the settings beside the symlink this process was
// launched through.
func loadFakeSettings() (fakeSettings, error) {
	path := fakeDirFile(fakeSettingsFile)

	raw, err := os.ReadFile(path) // #nosec G304 -- a fixture path beside the fake
	if err != nil {
		return fakeSettings{}, fmt.Errorf("read %s: %w", path, err)
	}

	var settings fakeSettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		return fakeSettings{}, fmt.Errorf("parse %s: %w", path, err)
	}

	return settings, nil
}

// reportStatus answers `bw status`.
func reportStatus(settings fakeSettings) int {
	status := settings.Status
	if status == "" {
		status = "unlocked"
	}

	fmt.Printf("{%q:%q}\n", "status", status)

	return 0
}

// reportLogin answers `bw login --apikey`.
func reportLogin(settings fakeSettings) int {
	if settings.LoginFails {
		fmt.Fprintln(os.Stderr, "login refused")

		return 1
	}

	fmt.Println("logged-in")

	return 0
}

// runFakeServe binds a real port because this runs in a re-executed child
// process standing in for `bw serve`, which the client reaches over TCP. It
// records every path it answers, so a case can assert that /lock ran even on a
// failure. Nothing here has to hurry: the client's probe decides when this
// daemon is up.
func runFakeServe(argv []string) int {
	settings, err := loadFakeSettings()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake bw: %v\n", err)

		return 3
	}

	attempt := serveAttempt(fakeDirFile(fakeArgvFileName))
	appendLine(fakeDirFile(fakeArgvFileName), strings.Join(argv, " "))

	if attempt <= settings.BusyServes {
		return refuseBind(settings.BusyPort)
	}

	if attempt <= settings.BusyServes+settings.SilentServes {
		// Up, holding no port, and never answering: the client's budget is what
		// must end its wait, and its cancel is what kills this process.
		select {}
	}

	//nolint:noctx // this runs in the re-executed fake process, which has no context to cancel
	listener, err := net.Listen("tcp", "127.0.0.1:"+flagValue(argv, "--port"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake serve: %v\n", err)

		return 1
	}

	server := &http.Server{Handler: fakeHandler(settings), ReadHeaderTimeout: headerTimeout}

	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "fake serve: %v\n", err)

		return 1
	}

	return 0
}

// serveAttempt counts the serve invocations already recorded, so the fake knows
// which try this is. The client starts them one at a time and every case has its
// own record, so a plain count is enough.
func serveAttempt(argvFile string) int {
	raw, err := os.ReadFile(argvFile) // #nosec G304 -- a fixture path beside the fake
	if err != nil {
		return 1
	}

	count := 1

	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.HasPrefix(line, "serve ") {
			count++
		}
	}

	return count
}

// refuseBind reproduces the race the client retries around: the port this child
// was given went to something else on the way here. Binding a port the case
// holds open puts the kernel's own refusal on stderr, which is what the client
// has to report.
func refuseBind(port int) int {
	//nolint:noctx // this runs in the re-executed fake process, which has no context to cancel
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake serve: %v\n", err)

		return 1
	}

	_ = listener.Close()

	fmt.Fprintf(os.Stderr, "fake serve: port %d was free after all\n", port)

	return 1
}

// stallCap is the backstop for a client disconnect this process never notices.
// A passing run never waits for it: the probe's own timeout ends the request
// first.
const stallCap = 5 * time.Second

// stall leaves one request unanswered until the caller gives up, which is what a
// dial the host drops costs the client.
func stall(request *http.Request) {
	select {
	case <-request.Context().Done():
	case <-time.After(stallCap):
	}
}

// fakeHandler answers one request, recording the path and, for a body, the key
// names only: a failing test must never print a value.
func fakeHandler(settings fakeSettings) http.Handler {
	var stalled atomic.Bool

	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if settings.StallFirstProbe && stalled.CompareAndSwap(false, true) {
			stall(request)

			return
		}

		appendLine(fakeDirFile(fakeCallsFileName), request.URL.Path)

		if request.Method == http.MethodPost {
			appendLine(fakeDirFile(fakeCallsFileName), "body:"+strings.Join(bodyKeys(request), ","))
		}

		writer.Header().Set("Content-Type", "application/json")

		if !strings.HasPrefix(request.URL.Path, "/list/object/items") {
			_, _ = writer.Write([]byte(`{"success":true,"data":{"template":{"status":"unlocked"}}}`))

			return
		}

		items, err := os.ReadFile(fakeDirFile(fakeItemsFileName)) // #nosec G304 -- a fixture path
		if err != nil {
			http.Error(writer, "no items fixture", http.StatusInternalServerError)

			return
		}

		_, _ = writer.Write(items)
	})
}

// bodyKeys returns the request body's top-level key names, which is enough to
// prove a field was sent without recording what it held.
func bodyKeys(request *http.Request) []string {
	var decoded map[string]any

	if err := json.NewDecoder(request.Body).Decode(&decoded); err != nil {
		return []string{"unparsed"}
	}

	keys := make([]string, 0, len(decoded))
	for key := range decoded {
		keys = append(keys, key)
	}

	return keys
}

// flagValue pulls one flag's value out of the fake's own argv.
func flagValue(argv []string, name string) string {
	for index := range argv {
		if argv[index] == name && index+1 < len(argv) {
			return argv[index+1]
		}
	}

	return ""
}

// appendLine records one line. A write that fails is dropped, because this runs
// in a child process with no way to report anything: the case reading the record
// back is what notices the gap.
func appendLine(path, line string) {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fixtureFilePerm) // #nosec G304 -- a fixture path
	if err != nil {
		return
	}

	defer func() { _ = file.Close() }()

	_, _ = fmt.Fprintln(file, line)
}

// fakeBW is one staged fake CLI: the binary path to hand the client, plus the
// record files a case reads back.
type fakeBW struct {
	binary string
	argv   string
	calls  string
}

// stageFakeBW writes the fixtures and symlinks this test binary as bw. The
// returned binary path is absolute, which is what the client gets from LookPath
// in production and what lets the child find its settings.
func stageFakeBW(t *testing.T, itemsJSON string, settings fakeSettings) fakeBW {
	t.Helper()

	dir := t.TempDir()
	fake := fakeBW{
		binary: filepath.Join(dir, fakeName),
		argv:   filepath.Join(dir, "argv.txt"),
		calls:  filepath.Join(dir, "calls.txt"),
	}

	writeFixture(t, filepath.Join(dir, fakeItemsFileName), itemsJSON, fixtureFilePerm)
	writeFixture(t, filepath.Join(dir, fakeSettingsFile), encodeSettings(t, settings), fixtureFilePerm)
	linkTestBinary(t, fake.binary)

	return fake
}

// encodeSettings renders the fake's configuration.
func encodeSettings(t *testing.T, settings fakeSettings) string {
	t.Helper()

	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("encode the fake settings: %v", err)
	}

	return string(raw)
}

// linkTestBinary makes path an executable that runs this same test binary. A
// symlink rather than a copy keeps it working when the temp directory is on
// another filesystem.
func linkTestBinary(t *testing.T, path string) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}

	if linkErr := os.Symlink(self, path); linkErr != nil {
		t.Fatalf("link the fake bw: %v", linkErr)
	}
}

// writeFixture writes one fixture file at the given mode.
func writeFixture(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// recorded reads one record file, returning nothing when the fake never wrote it.
func recorded(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) // #nosec G304 -- the path is built by the test
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}

	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(data)
}

// listening reports whether anything still answers on the daemon's port, which
// is how a case proves the daemon was stopped rather than just locked.
func listening(ctx context.Context, port string) bool {
	dialer := net.Dialer{Timeout: time.Second}

	conn, err := dialer.DialContext(ctx, "tcp", "127.0.0.1:"+port)
	if err != nil {
		return false
	}

	_ = conn.Close()

	return true
}

// servePorts lists the ports the fake was told to listen on, in the order it was
// started, so a case can see a start move to another port.
func servePorts(argvRecord string) []string {
	var ports []string

	for line := range strings.SplitSeq(argvRecord, "\n") {
		if !strings.HasPrefix(line, "serve ") {
			continue
		}

		if port := flagValue(strings.Fields(line), "--port"); port != "" {
			ports = append(ports, port)
		}
	}

	return ports
}

// servePort is the last port the fake was given, which is the daemon a case
// checks after the call returned.
func servePort(t *testing.T, argvRecord string) string {
	t.Helper()

	ports := servePorts(argvRecord)
	if len(ports) == 0 {
		t.Fatalf("no serve invocation in the argv record:\n%s", argvRecord)
	}

	return ports[len(ports)-1]
}

// occupiedPort holds a loopback port open for as long as the case runs, so the
// fake has something to fail against.
func occupiedPort(t *testing.T) int {
	t.Helper()

	var config net.ListenConfig

	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a loopback port: %v", err)
	}

	t.Cleanup(func() { _ = listener.Close() })

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("the held listener reported no TCP port")
	}

	return addr.Port
}

// itemsJSON renders the list response for one item with the given fields, in the
// double-nested envelope bw serve uses.
func itemsJSON(entries ...string) string {
	return `{"success":true,"data":{"data":[` + strings.Join(entries, ",") + `]}}`
}

// itemJSON renders one item with its custom fields.
func itemJSON(name string, fields map[string]string) string {
	rendered := make([]string, 0, len(fields))
	for fieldName, value := range fields {
		rendered = append(rendered, `{"name":"`+fieldName+`","value":"`+value+`"}`)
	}

	return `{"name":"` + name + `","fields":[` + strings.Join(rendered, ",") + `]}`
}
