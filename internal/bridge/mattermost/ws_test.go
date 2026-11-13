package mattermost_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/chadit/CairnOS/internal/bridge/mattermost"
	"github.com/chadit/CairnOS/internal/nettest"
)

// The recorded frames under testdata.
const (
	fixtureHello         = "hello.json"
	fixtureDM            = "posted_dm.json"
	fixtureRootMention   = "posted_channel_mention_root.json"
	fixtureThreadMention = "posted_channel_mention_thread.json"
	fixtureBotPlain      = "posted_bot_plain.json"
	fixtureBotActivate   = "posted_bot_activate.json"
	fixtureCodeFence     = "posted_code_fence.json"
	fixtureSystemJoin    = "posted_system_join.json"
	fixtureOwnEcho       = "posted_own_echo.json"
	fixtureMalformed     = "posted_malformed.json"
)

const (
	// resultBufferSize gives Listen's terminal error a slot, so its goroutine
	// exits even when the test stops waiting first.
	resultBufferSize = 1

	// strayEventSize gives a Listen that should emit nothing room for one event,
	// so a wrong emit lands in the channel instead of blocking Listen.
	strayEventSize = 1
)

// replayOrder is the order the replay server writes the frames. Two of them are
// frames the bridge must ignore rather than act on.
func replayOrder() []string {
	return []string{
		fixtureHello,
		fixtureDM,
		fixtureRootMention,
		fixtureThreadMention,
		fixtureBotPlain,
		fixtureBotActivate,
		fixtureCodeFence,
		fixtureSystemJoin,
		fixtureOwnEcho,
		fixtureMalformed,
	}
}

func loadFixtures(t *testing.T, names []string) [][]byte {
	t.Helper()

	frames := make([][]byte, 0, len(names))

	for _, name := range names {
		frame, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("read fixture %s: %v", name, err)
		}

		frames = append(frames, frame)
	}

	return frames
}

// newSocketServer replays frames over one WebSocket connection. abrupt drops
// the connection without a close handshake, which is what a server restart
// looks like to the reader.
func newSocketServer(t *testing.T, frames [][]byte, abrupt bool) *nettest.Server {
	t.Helper()

	server := nettest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}

		defer dropFixtureConn(t, conn)

		if writeFrames(r.Context(), conn, frames) != nil {
			return
		}

		if !abrupt {
			_ = conn.Close(websocket.StatusNormalClosure, "")
		}
	}))

	return server
}

// dropFixtureConn closes the server side of a fixture socket without a
// handshake. The client may have closed it first, which reports net.ErrClosed
// and is not a fixture failure.
func dropFixtureConn(t *testing.T, conn *websocket.Conn) {
	t.Helper()

	if err := conn.CloseNow(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Errorf("close fixture socket: %v", err)
	}
}

func writeFrames(ctx context.Context, conn *websocket.Conn, frames [][]byte) error {
	for _, frame := range frames {
		if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
			return fmt.Errorf("write frame: %w", err)
		}
	}

	return nil
}

// replay runs Listen against a server that writes frames and then hangs up,
// returning every event the parser emitted plus the error Listen ended on.
func replay(t *testing.T, names []string, abrupt bool) ([]mattermost.Event, error) {
	t.Helper()

	frames := loadFixtures(t, names)
	server := newSocketServer(t, frames, abrupt)
	client := newTestClient(t, server, nil)

	events := make(chan mattermost.Event, len(frames))
	done := make(chan error, resultBufferSize)

	go func() { done <- client.Listen(t.Context(), events) }()

	err := <-done

	close(events)

	got := make([]mattermost.Event, 0, len(frames))
	for event := range events {
		got = append(got, event)
	}

	return got, err
}

func TestListenEmitsOnlyPostedEvents(t *testing.T) {
	t.Parallel()

	events, err := replay(t, replayOrder(), false)
	if err == nil {
		t.Fatal("Listen returned no error after the server hung up; the caller needs one to reconnect")
	}

	// Ten frames in, eight out: hello is not a post, and the malformed payload
	// is dropped rather than passed on half-decoded.
	if len(events) != 8 {
		t.Fatalf("Listen emitted %d events, want 8 of the 10 recorded frames", len(events))
	}

	for index, event := range events {
		if event.Name != mattermost.EventPosted {
			t.Fatalf("event %d is %q, want only posted events", index, event.Name)
		}

		if event.Post == nil {
			t.Fatalf("event %d carries no post", index)
		}
	}
}

func TestListenParsesTheRecordedFrames(t *testing.T) {
	t.Parallel()

	// The replay server always hangs up, so Listen always ends on an error; the
	// events it produced first are what this case reads.
	events, replayErr := replay(t, replayOrder(), false)
	if replayErr == nil {
		t.Fatal("Listen returned no error after the server hung up")
	}

	byID := make(map[string]mattermost.Event, len(events))
	for _, event := range events {
		byID[event.Post.ID] = event
	}

	direct, found := byID["pstdm1k4x9wqz2mtn7rc5vhbdp"]
	if !found {
		t.Fatal("the DM frame produced no event")
	}

	if direct.ChannelType != mattermost.ChannelDirect || len(direct.Mentions) != 0 {
		t.Fatalf("DM event = %+v, want a direct channel with no mentions", direct)
	}

	if direct.Post.Message != "what is the deploy status?" || direct.SenderName != "@alice" {
		t.Fatalf("DM event lost the message or the sender: %+v", direct.Post)
	}

	root, found := byID["pstroot4m9kx2wqz7tdn5cvbhp"]
	if !found {
		t.Fatal("the channel mention frame produced no event")
	}

	if !slices.Contains(root.Mentions, testBotID) {
		t.Fatalf("mentions = %v, want the bot's own id: a connection only ever sees itself", root.Mentions)
	}

	if root.Post.RootID != "" {
		t.Fatalf("root post carries root_id %q, want it empty", root.Post.RootID)
	}

	thread, found := byID["pstthr8n3vq6kxz4wmd7cjtprh"]
	if !found {
		t.Fatal("the thread mention frame produced no event")
	}

	if thread.Post.RootID != root.Post.ID {
		t.Fatalf("thread reply root_id = %q, want the root post id", thread.Post.RootID)
	}

	system, found := byID["pstsys9q4kx2wz7mnt3rvdhcpj"]
	if !found {
		t.Fatal("the system frame produced no event")
	}

	if system.Post.Type == "" {
		t.Fatal("the system post decoded with an empty type, which is the field the gate drops it on")
	}

	echo, found := byID["pstown3k7qz9wx2mnt5rvdhcpj"]
	if !found {
		t.Fatal("the own-echo frame produced no event")
	}

	if echo.Post.UserID != testBotID {
		t.Fatalf("own echo user_id = %q, want the bot's own id", echo.Post.UserID)
	}
}

func TestListenKeepsStringPropsAndDropsTheRest(t *testing.T) {
	t.Parallel()

	events, replayErr := replay(t, []string{fixtureBotPlain}, false)
	if replayErr == nil {
		t.Fatal("Listen returned no error after the server hung up")
	}

	if len(events) != 1 {
		t.Fatalf("got %d events, want the one recorded webhook post", len(events))
	}

	props := events[0].Post.Props

	if props[propFromWebhook] != propTrue {
		t.Fatalf("props = %v, want from_webhook carried through", props)
	}

	// The recorded post nests an attachments array under props. A decode that
	// insisted on string values would have failed the whole post.
	if _, present := props["attachments"]; present {
		t.Fatalf("props = %v, want the non-string attachments entry dropped", props)
	}
}

func TestListenReturnsOnAMidStreamDrop(t *testing.T) {
	t.Parallel()

	events, err := replay(t, []string{fixtureDM, fixtureRootMention}, true)
	if err == nil {
		t.Fatal("Listen returned no error after the connection dropped")
	}

	if len(events) == 0 {
		t.Fatal("Listen dropped the frames it had already read")
	}

	if class := classOf(t, err); class != mattermost.ClassTransient {
		t.Fatalf("a dropped connection classified as %v, want transient so the caller reconnects", class)
	}
}

func TestListenRefusesAServerThatRejectsTheToken(t *testing.T) {
	t.Parallel()

	server := nettest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))

	client := newTestClient(t, server, nil)

	events := make(chan mattermost.Event, strayEventSize)

	err := client.Listen(t.Context(), events)
	if err == nil {
		t.Fatal("Listen against a server that refused the upgrade returned no error")
	}

	if len(events) != 0 {
		t.Fatal("Listen emitted an event without a connection")
	}
}

func TestListenStopsWhenTheContextEnds(t *testing.T) {
	t.Parallel()

	// A server that accepts the upgrade and then says nothing, so only the
	// context can end the read.
	server := nettest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}

		defer dropFixtureConn(t, conn)

		<-r.Context().Done()
	}))

	client := newTestClient(t, server, nil)

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, resultBufferSize)
	go func() { done <- client.Listen(ctx, make(chan mattermost.Event)) }()

	cancel()

	if err := <-done; err == nil {
		t.Fatal("Listen returned no error after its context ended")
	}
}

// TestRecordedFramesDriveTheGate walks each fixture through the gate's pure
// checks, so a change to the parser or to the mention rules shows up against
// real server output rather than a hand-built struct.
func TestRecordedFramesDriveTheGate(t *testing.T) {
	t.Parallel()

	events, replayErr := replay(t, replayOrder(), false)
	if replayErr == nil {
		t.Fatal("Listen returned no error after the server hung up")
	}

	emitted := postedFixtures()

	// One event per posted fixture, in order. A parser regression that drops a
	// frame has to fail here, naming the parser, rather than panic on an index
	// further down.
	if len(events) != len(emitted) {
		t.Fatalf("replay produced %d events, want one per posted fixture (%d)", len(events), len(emitted))
	}

	byName := make(map[string]mattermost.Event, len(emitted))
	for index, name := range emitted {
		byName[name] = events[index]
	}

	cases := []struct {
		name          string
		fixture       string
		wantMention   bool
		wantAutomated bool
		wantActivate  bool
	}{
		{name: "dm needs no mention", fixture: fixtureDM},
		{name: "root mention", fixture: fixtureRootMention, wantMention: true},
		{name: "thread mention", fixture: fixtureThreadMention, wantMention: true},
		{name: "webhook post", fixture: fixtureBotPlain, wantMention: true, wantAutomated: true},
		{name: "bot opting in", fixture: fixtureBotActivate, wantMention: true, wantAutomated: true, wantActivate: true},
		{name: "mention only inside a fence", fixture: fixtureCodeFence},
		{name: "system post", fixture: fixtureSystemJoin},
		{name: "own echo", fixture: fixtureOwnEcho, wantAutomated: true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			event, found := byName[testCase.fixture]
			if !found {
				t.Fatalf("fixture %s produced no event", testCase.fixture)
			}

			if got := mattermost.MentionsInText(event.Post.Message, testBotTag); got != testCase.wantMention {
				t.Fatalf("MentionsInText(%q) = %v, want %v", event.Post.Message, got, testCase.wantMention)
			}

			if got := mattermost.IsAutomated(event.Post, nil); got != testCase.wantAutomated {
				t.Fatalf("IsAutomated(props=%v) = %v, want %v", event.Post.Props, got, testCase.wantAutomated)
			}

			if got := mattermost.ActivateAI(event.Post); got != testCase.wantActivate {
				t.Fatalf("ActivateAI(props=%v) = %v, want %v", event.Post.Props, got, testCase.wantActivate)
			}
		})
	}
}

// postedFixtures lists the recorded frames that reach a caller, in replay
// order: the hello frame and the malformed payload never emit an event.
func postedFixtures() []string {
	order := replayOrder()
	emitted := make([]string, 0, len(order))

	for _, name := range order {
		if name == fixtureHello || name == fixtureMalformed {
			continue
		}

		emitted = append(emitted, name)
	}

	return emitted
}

// newDeafServer accepts the upgrade and then answers nothing, not even a pong.
// coder/websocket replies to a ping from inside Read, so a handler that never
// reads is exactly what a NAT table dropping an idle flow looks like: the socket
// is still open and nothing will ever come back.
func newDeafServer(t *testing.T) *nettest.Server {
	t.Helper()

	server := nettest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}

		defer dropFixtureConn(t, conn)

		<-r.Context().Done()
	}))

	return server
}

// TestListenFailsWhenTheSocketStopsAnsweringPings proves the keepalive turns a
// half-open connection into a transient failure the reconnect loop acts on.
// Without it Read blocks forever, so the identity stops answering DMs and
// mentions with no error and no log line until the process restarts.
func TestListenFailsWhenTheSocketStopsAnsweringPings(t *testing.T) {
	t.Parallel()

	server := newDeafServer(t)

	client, err := mattermost.NewClientOver(mattermost.ClientConfig{
		URL:               server.URL,
		Token:             testToken,
		InsecureHTTP:      true,
		Log:               quietLog(),
		KeepaliveInterval: 30 * time.Millisecond,
	}, server.Transport())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	events := make(chan mattermost.Event, strayEventSize)
	done := make(chan error, resultBufferSize)

	go func() { done <- client.Listen(t.Context(), events) }()

	select {
	case listenErr := <-done:
		assertKeepaliveFailure(t, listenErr)
	case <-time.After(10 * time.Second):
		t.Fatal("Listen never returned: a socket that stopped answering pings went undetected")
	}
}

// assertKeepaliveFailure checks that the probe, not the read it cut short, is
// what Listen reported, and that the class tells the caller to reconnect.
func assertKeepaliveFailure(t *testing.T, err error) {
	t.Helper()

	failure, ok := errors.AsType[*mattermost.APIError](err)
	if !ok || failure.Class != mattermost.ClassTransient {
		t.Fatalf("Listen error = %v, want a transient APIError the backoff reconnects from", err)
	}

	if !strings.Contains(failure.Message, "keepalive") {
		t.Fatalf("Listen error = %q, want the keepalive named as the cause", failure.Message)
	}
}
