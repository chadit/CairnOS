package cli_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/nettest"
)

// serveFailedStateName is the file the repeat window lives in, mirrored from the
// package so a test can seed and inspect it.
const serveFailedStateName = "serve-failed.json"

// notifyChannelID is the id the stub server resolves #alerts to.
const notifyChannelID = "chn1"

// mattermostKeyID is the key Mattermost's API returns an object's ID under.
const mattermostKeyID = "id"

// serveFailedConfigTOML pairs a source serve cannot bring up with a Mattermost
// notify destination, so a startup that dies before the bridges connect still
// has somewhere to report. The verbs take the stub server URL and the checkout
// the source block points at.
const serveFailedConfigTOML = `
[api]
grpc = "127.0.0.1:0"
http = "127.0.0.1:0"

[telemetry]
enabled = false

[bridge.mattermost]
url           = "%s"
team          = "main"
insecure_http = true

[bridge.mattermost.identity.ops]
username  = "cairn"
workspace = "default"

[notify.mattermost]
identity = "ops"
channel  = "#alerts"
events   = ["serve_failed"]

[source.automation]
repo = "https://example.invalid/automation.git"
dest = "%s"
ref  = "main"
`

// fakeMattermost answers the two calls one notify post makes, resolving #alerts
// and creating the post, and hands every posted message back on the channel.
// It answers in the process through the bridge transport, because a loopback
// stub can sit on a port with stale connection tracking.
func fakeMattermost(t *testing.T) (http.Handler, chan string) {
	t.Helper()

	posts := make(chan string, postCaptureSize)
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v4/teams/name/main/channels/name/alerts", func(w http.ResponseWriter, _ *http.Request) {
		writeMattermostJSON(t, w, map[string]string{mattermostKeyID: notifyChannelID})
	})

	mux.HandleFunc("/api/v4/posts", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Message string `json:"message"`
		}

		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode post body: %v", err)
		}

		posts <- body.Message

		writeMattermostJSON(t, w, map[string]string{mattermostKeyID: "p1", "channel_id": notifyChannelID})
	})

	return mux, posts
}

// writeMattermostJSON answers one stub request. A failure here means the stub
// broke rather than the code under test, so it fails the case.
func writeMattermostJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encode stub response: %v", err)
	}
}

// serveFailedDir writes a home whose source block points at checkout, which is a
// directory rather than a work tree, so startup fails at the sources stage.
func serveFailedDir(t *testing.T, dir, checkout string) {
	t.Helper()

	cfg := fmt.Sprintf(serveFailedConfigTOML, nettest.BaseURL, checkout)

	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "workflows"), 0o750); err != nil {
		t.Fatalf("create workflows dir: %v", err)
	}
}

// awaitPost waits for one posted line and returns it.
func awaitPost(t *testing.T, posts <-chan string) string {
	t.Helper()

	select {
	case line := <-posts:
		return line
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the serve failure notice")

		return ""
	}
}

// TestServeFailureNotifiesOnceThenAgainOnANewFailure proves the crash-loop
// posture: a startup failure announces itself, the same failure on the next
// restart stays quiet, and a different failure gets through at once.
func TestServeFailureNotifiesOnceThenAgainOnANewFailure(t *testing.T) {
	t.Parallel()

	env := withSourcePath(tokenEnv(mattermostTokenEnv, fakeBridgeToken))
	stub, posts := fakeMattermost(t)
	dir := t.TempDir()

	serve := func() (int, string, string) {
		return runConfiguredCLI(t, func(app *cli.App) *cli.App {
			return app.WithEnv(environ.Map(env)).WithBridgeTransport(nettest.Transport(stub))
		}, serveCmd, flagDirArg, dir)
	}

	serveFailedDir(t, dir, t.TempDir())

	if code, _, stderr := serve(); code != cli.ExitFailure {
		t.Fatalf("serve exit = %d, want failure\nstderr: %s", code, stderr)
	}

	line := awaitPost(t, posts)
	for _, want := range []string{"serve failed at sources", "automation"} {
		if !strings.Contains(line, want) {
			t.Fatalf("notice %q is missing %q", line, want)
		}
	}

	if code, _, _ := serve(); code != cli.ExitFailure {
		t.Fatalf("the second start exit = %d, want failure", code)
	}

	assertNoSecondPost(t, posts)

	// A different checkout path is a different failure, so the window it was
	// holding no longer applies.
	serveFailedDir(t, dir, t.TempDir())

	if code, _, _ := serve(); code != cli.ExitFailure {
		t.Fatalf("the third start exit = %d, want failure", code)
	}

	if next := awaitPost(t, posts); !strings.Contains(next, "serve failed at sources") {
		t.Fatalf("the changed failure posted %q", next)
	}
}

// TestServeStartClearsTheFailureWindow proves a healthy start drops the window,
// so the next failure after a working run is reported rather than swallowed as
// the tail of an old restart loop.
func TestServeStartClearsTheFailureWindow(t *testing.T) {
	t.Parallel()

	dir := bridgeServeDir(t, serveConfigTOML)
	statePath := filepath.Join(dir, serveFailedStateName)

	seeded := fmt.Sprintf(`{"sent":%q,"error":"whatever-went-wrong"}`, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(statePath, []byte(seeded), 0o600); err != nil {
		t.Fatalf("seed the failure state: %v", err)
	}

	_, _, exit, cancel := serveUntilReady(t, dir, nil)

	cancel()

	if code := <-exit; code != cli.ExitOK {
		t.Fatalf("serve exit = %d, want %d", code, cli.ExitOK)
	}

	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("a healthy start left the failure state behind: %v", err)
	}
}
