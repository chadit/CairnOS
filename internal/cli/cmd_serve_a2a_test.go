package cli_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	a2av1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/a2a/v1"
	"github.com/chadit/CairnOS/internal/cli"
)

// a2aCardPath is the public discovery path, mirrored from the a2a package so a
// peer's view of the card is what gets asserted here.
const a2aCardPath = "/.well-known/agent.json"

// transportMattermost is the transport label a Mattermost identity carries on
// the card and in a messaging listing.
const transportMattermost = "mattermost"

// cardFetchTimeout bounds the card fetch so a server that binds but never
// answers fails the case instead of hanging the suite until the package timeout.
const cardFetchTimeout = 5 * time.Second

// a2aNoChatTOML enables A2A with chat off, so SendMessage has no core to run a
// turn through.
const a2aNoChatTOML = `
[api]
grpc = "127.0.0.1:0"
http = "127.0.0.1:0"

[telemetry]
enabled    = true
prometheus = "127.0.0.1:0"

[a2a]
enabled = true
`

// a2aWithIdentityTOML enables A2A alongside chat and one Mattermost identity,
// the wiring whose card advertises a channel.
const a2aWithIdentityTOML = `
[api]
grpc = "127.0.0.1:0"
http = "127.0.0.1:0"

[telemetry]
enabled    = true
prometheus = "127.0.0.1:0"

[a2a]
enabled = true

[chat]
enabled = true
driver  = "openai"

[bridge.mattermost]
url  = "https://mm.example.test"
team = "main"

[bridge.mattermost.identity.ops]
username     = "cairn"
display_name = "Ops Agent"
workspace    = "default"
`

// cardChannel is the card's channel entry as a peer parses it.
type cardChannel struct {
	Type        string `json:"type"`
	URL         string `json:"url"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

// TestServeA2ASendMessageNeedsChat proves the chat-disabled path reaches the
// wire: with no [chat] block the bridge core is never built, so serve passes no
// handler to the A2A bundle and SendMessage answers FailedPrecondition instead
// of pretending a turn ran. This is the assertion that fails if the wiring ever
// hands the bundle a typed-nil core.
func TestServeA2ASendMessageNeedsChat(t *testing.T) {
	t.Parallel()

	dir := bridgeServeDir(t, a2aNoChatTOML)

	stdout, _, exit, cancel := serveUntilReady(t, dir)

	grpcAddr := outputAddr(t, stdout, grpcAddrPattern, "gRPC")

	_, err := a2av1.NewA2AServiceClient(dialServe(t, grpcAddr)).SendMessage(t.Context(),
		&a2av1.SendMessageRequest{Text: "are you there"})

	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("SendMessage code = %v (err %v), want %v", got, err, codes.FailedPrecondition)
	}

	cancel()

	if code := <-exit; code != cli.ExitOK {
		t.Fatalf("serve exit = %d, want %d", code, cli.ExitOK)
	}
}

// TestServeA2ACardCarriesChannels proves the card a peer fetches advertises the
// configured chat identity, which is what tells a peer where to reach this node
// outside A2A. It also pins the ordering the wiring depends on: the card is
// signed at bundle build, so an identity registered after that would publish an
// empty channel list.
func TestServeA2ACardCarriesChannels(t *testing.T) {
	t.Setenv(mattermostTokenEnv, "fake-not-a-real-token")

	dir := bridgeServeDir(t, a2aWithIdentityTOML)

	stdout, _, exit, cancel := serveUntilReady(t, dir)

	httpAddr := outputAddr(t, stdout, httpAddrPattern, "HTTP")
	channels := fetchCardChannels(t, "http://"+httpAddr+a2aCardPath)

	cancel()

	if code := <-exit; code != cli.ExitOK {
		t.Fatalf("serve exit = %d, want %d", code, cli.ExitOK)
	}

	if len(channels) != 1 {
		t.Fatalf("card channels = %+v, want exactly one entry", channels)
	}

	want := cardChannel{
		Type:        transportMattermost,
		URL:         "https://mm.example.test",
		Username:    "cairn",
		DisplayName: "Ops Agent",
	}

	if channels[0] != want {
		t.Fatalf("card channel = %+v, want %+v", channels[0], want)
	}
}

// fetchCardChannels reads the published card and returns its channel list.
func fetchCardChannels(t *testing.T, cardURL string) []cardChannel {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, cardURL, http.NoBody)
	if err != nil {
		t.Fatalf("build card request: %v", err)
	}

	client := &http.Client{Timeout: cardFetchTimeout}

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("fetch card: %v", err)
	}

	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("card status = %d, want 200", response.StatusCode)
	}

	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read card: %v", err)
	}

	var card struct {
		Channels []cardChannel `json:"channels"`
	}

	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatalf("decode card %s: %v", raw, err)
	}

	return card.Channels
}
