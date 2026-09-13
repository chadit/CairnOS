package cli_test

import (
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
)

// mattermostTokenEnv is the env key the ops identity's bot token rides, mirrored
// from config.MattermostTokenEnv so the tests can set and clear it.
const mattermostTokenEnv = "CAIRNOS_BRIDGE_MATTERMOST_OPS_TOKEN" // #nosec G101 -- env var name, not a credential

// mattermostWithChatTOML declares one identity alongside an enabled chat agent,
// the wiring that starts a connection once a token is present.
const mattermostWithChatTOML = `
[api]
grpc = "127.0.0.1:0"
http = "127.0.0.1:0"

[telemetry]
enabled    = true
prometheus = "127.0.0.1:0"

[chat]
enabled = true
driver  = "openai"

[bridge.mattermost]
url  = "https://mm.example.test"
team = "main"

[bridge.mattermost.identity.ops]
username  = "cairn"
workspace = "default"
`

// mattermostNoChatTOML declares an identity while chat stays off, the
// cross-check serve must reject at wiring once the token is set.
const mattermostNoChatTOML = `
[api]
grpc = "127.0.0.1:0"
http = "127.0.0.1:0"

[telemetry]
enabled = false

[bridge.mattermost]
url = "https://mm.example.test"

[bridge.mattermost.identity.ops]
username  = "cairn"
workspace = "default"
`

// TestServeSkipsMattermostWhenNoIdentity proves the default posture: a config
// with no [bridge.mattermost.identity] block opens no connection and says so,
// while serve runs normally (FR2).
func TestServeSkipsMattermostWhenNoIdentity(t *testing.T) {
	t.Parallel()

	dir := bridgeServeDir(t, serveConfigTOML)

	stdout, stderr, exit, cancel := serveUntilReady(t, dir)

	cancel()

	if code := <-exit; code != cli.ExitOK {
		t.Fatalf("serve exit = %d, want %d", code, cli.ExitOK)
	}

	if strings.Contains(stdout.String(), "mattermost bridge running") {
		t.Fatalf("mattermost banner printed with no identity declared:\n%s", stdout.String())
	}

	if !strings.Contains(stderr.String(), "no identity declared") {
		t.Fatalf("stderr missing the no-identity log line:\n%s", stderr.String())
	}
}

// TestServeSkipsMattermostIdentityWithoutToken proves a declared identity whose
// token env is unset starts nothing, logs one line naming the env an operator
// has to set, and leaves serve running (FR2).
func TestServeSkipsMattermostIdentityWithoutToken(t *testing.T) {
	// Force the token empty regardless of ambient env; t.Setenv forbids Parallel.
	t.Setenv(mattermostTokenEnv, "")

	dir := bridgeServeDir(t, mattermostWithChatTOML)

	stdout, stderr, exit, cancel := serveUntilReady(t, dir)

	cancel()

	if code := <-exit; code != cli.ExitOK {
		t.Fatalf("serve exit = %d, want %d", code, cli.ExitOK)
	}

	if strings.Contains(stdout.String(), "mattermost bridge running") {
		t.Fatalf("mattermost banner printed with the token unset:\n%s", stdout.String())
	}

	if !strings.Contains(stderr.String(), mattermostTokenEnv) {
		t.Fatalf("stderr does not name the token env an operator must set:\n%s", stderr.String())
	}
}

// TestServeFailsWhenMattermostWithoutChat proves the cross-check: a declared
// identity with chat off fails serve startup with config.ErrInvalidConfig,
// because the bridge has no agent to drive (FR4).
func TestServeFailsWhenMattermostWithoutChat(t *testing.T) {
	t.Setenv(mattermostTokenEnv, "fake-not-a-real-token")

	dir := bridgeServeDir(t, mattermostNoChatTOML)

	code, _, stderr := runCLI(t, serveCmd, flagDirArg, dir)

	if code != cli.ExitFailure {
		t.Fatalf("serve exit = %d, want failure\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stderr, "requires [chat] enabled") {
		t.Fatalf("stderr missing the cross-check error:\n%s", stderr)
	}

	if !strings.Contains(stderr, "invalid configuration") {
		t.Fatalf("stderr does not report the failure as a config error:\n%s", stderr)
	}
}

// TestServePrintsMessagingEndpoint proves the send route is mounted on every
// serve, not only when an identity is configured, so a caller learns the path
// from the startup banner rather than the swagger.
func TestServePrintsMessagingEndpoint(t *testing.T) {
	t.Parallel()

	dir := bridgeServeDir(t, serveConfigTOML)

	stdout, _, exit, cancel := serveUntilReady(t, dir)

	cancel()

	if code := <-exit; code != cli.ExitOK {
		t.Fatalf("serve exit = %d, want %d", code, cli.ExitOK)
	}

	if !strings.Contains(stdout.String(), "messaging endpoint at http://127.0.0.1:") {
		t.Fatalf("stdout missing the messaging endpoint banner:\n%s", stdout.String())
	}
}
