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
	"strings"
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

// fakeSettings is the fake's whole configuration, written next to the symlink.
type fakeSettings struct {
	// Status is what `bw status` reports; empty means unlocked.
	Status string `json:"status"`

	// ItemsFile holds the JSON body the list endpoint returns.
	ItemsFile string `json:"items_file"`

	// ArgvFile and CallsFile are the two records a case reads back.
	ArgvFile  string `json:"argv_file"`
	CallsFile string `json:"calls_file"`

	// NoServe makes `bw serve` stay up without binding, the shape of a daemon
	// that never becomes ready.
	NoServe bool `json:"no_serve"`

	// LoginFails makes `bw login` exit non-zero.
	LoginFails bool `json:"login_fails"`
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
	settings, err := loadFakeSettings()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake bw: %v\n", err)

		return 3
	}

	appendLine(settings.ArgvFile, strings.Join(argv, " "))

	if len(argv) == 0 {
		return 2
	}

	switch argv[0] {
	case "status":
		return reportStatus(settings)
	case "login":
		return reportLogin(settings)
	case "serve":
		return runFakeServe(settings, argv)
	default:
		fmt.Fprintf(os.Stderr, "unexpected subcommand: %s\n", argv[0])

		return 2
	}
}

// loadFakeSettings reads the settings beside the symlink this process was
// launched through.
func loadFakeSettings() (fakeSettings, error) {
	path := filepath.Join(filepath.Dir(os.Args[0]), fakeSettingsFile)

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

// runFakeServe answers the endpoints the client drives, recording every path so
// a case can assert that /lock ran even on a failure.
func runFakeServe(settings fakeSettings, argv []string) int {
	if settings.NoServe {
		// Never listen and never exit on our own: the client's readiness
		// deadline is what must end this wait, and its cancel kills the child.
		select {}
	}

	server := &http.Server{
		Addr:              "127.0.0.1:" + flagValue(argv, "--port"),
		Handler:           fakeHandler(settings),
		ReadHeaderTimeout: headerTimeout,
	}

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "fake serve: %v\n", err)

		return 1
	}

	return 0
}

// fakeHandler answers one request, recording the path and, for a body, the key
// names only: a failing test must never print a value.
func fakeHandler(settings fakeSettings) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		appendLine(settings.CallsFile, request.URL.Path)

		if request.Method == http.MethodPost {
			appendLine(settings.CallsFile, "body:"+strings.Join(bodyKeys(request), ","))
		}

		writer.Header().Set("Content-Type", "application/json")

		if !strings.HasPrefix(request.URL.Path, "/list/object/items") {
			_, _ = writer.Write([]byte(`{"success":true,"data":{"template":{"status":"unlocked"}}}`))

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

// appendLine records one line, ignoring an unset path so a case that asked for
// no record simply gets none.
func appendLine(path, line string) {
	if path == "" {
		return
	}

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

	settings.ItemsFile = filepath.Join(dir, "items.json")
	settings.ArgvFile = fake.argv
	settings.CallsFile = fake.calls

	writeFixture(t, settings.ItemsFile, itemsJSON, fixtureFilePerm)
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

// servePort reads the port the fake was told to listen on out of its argv
// record, so a case can check that port afterwards.
func servePort(t *testing.T, argvRecord string) string {
	t.Helper()

	for line := range strings.SplitSeq(argvRecord, "\n") {
		if !strings.HasPrefix(line, "serve ") {
			continue
		}

		if port := flagValue(strings.Fields(line), "--port"); port != "" {
			return port
		}
	}

	t.Fatalf("no serve invocation in the argv record:\n%s", argvRecord)

	return ""
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
