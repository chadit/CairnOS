package mattermost_test

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/bridge"
	"github.com/chadit/CairnOS/internal/bridge/bridgetest"
	"github.com/chadit/CairnOS/internal/bridge/mattermost"
	"github.com/chadit/CairnOS/internal/config"
)

// Fixture identity and server values every case addresses.
const (
	identityOps  = "ops"
	botUserID    = "bot-ops"
	botUsername  = "cairn"
	humanUserID  = "user-1"
	humanName    = "alice"
	channelID    = "chan-1"
	channelName  = "agents"
	teamName     = "main"
	serverURL    = "https://mm.example.test"
	workspaceOps = "default"

	// rootPostID is the post most cases mention or reply under.
	rootPostID = "root-1"
)

// recordedPost is one CreatePost the fake server accepted.
type recordedPost struct {
	ChannelID string
	RootID    string
	Message   string
}

// recordedReaction is one AddReaction the fake server saw, failed or not.
type recordedReaction struct {
	PostID string
	Emoji  string
}

// fakeConn is a scripted mattermost.Conn: it replays whatever a test pushes,
// records every post, and answers the lookups the gate and the send path make.
// It is the second implementation of Conn, which is what keeps that interface
// from being a wrapper around the one concrete client.
type fakeConn struct {
	inbox    chan mattermost.Event
	users    map[string]mattermost.User
	channels map[string]mattermost.Channel

	// reactErr, when set, is what every AddReaction returns after recording.
	reactErr error

	posts []recordedPost
	me    mattermost.User

	reactions []recordedReaction

	mu     sync.Mutex
	typing int
	postNo int
}

// newFakeConn builds a connection answering as one bot account.
func newFakeConn(userID, username string) *fakeConn {
	return &fakeConn{
		inbox:    make(chan mattermost.Event, 16),
		users:    map[string]mattermost.User{},
		channels: map[string]mattermost.Channel{},
		me:       mattermost.User{ID: userID, Username: username},
	}
}

func (*fakeConn) Connect(_ context.Context) error { return nil }

func (c *fakeConn) Me() mattermost.User { return c.me }

func (*fakeConn) MaxPostSize() int { return 4000 }

// Listen forwards pushed events until ctx ends, the way the real socket reader
// does, so the bridge's pump loop is exercised rather than bypassed.
func (c *fakeConn) Listen(ctx context.Context, events chan<- mattermost.Event) error {
	for {
		event, live := c.next(ctx)
		if !live {
			return fmt.Errorf("fake listen: %w", ctx.Err())
		}

		if !forward(ctx, events, &event) {
			return fmt.Errorf("fake listen: %w", ctx.Err())
		}
	}
}

// forward hands one event to the bridge or reports that ctx ended.
func forward(ctx context.Context, events chan<- mattermost.Event, event *mattermost.Event) bool {
	select {
	case <-ctx.Done():
		return false
	case events <- *event:
		return true
	}
}

func (c *fakeConn) CreatePost(_ context.Context, target, rootID, message string) (mattermost.Post, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.postNo++
	c.posts = append(c.posts, recordedPost{ChannelID: target, RootID: rootID, Message: message})

	return mattermost.Post{
		ID: "reply-" + strconv.Itoa(c.postNo), UserID: c.me.ID,
		ChannelID: target, RootID: rootID, Message: message,
	}, nil
}

func (*fakeConn) DirectChannel(_ context.Context, otherUserID string) (mattermost.Channel, error) {
	return mattermost.Channel{ID: "dm-" + otherUserID, Type: mattermost.ChannelDirect}, nil
}

func (*fakeConn) UserByUsername(_ context.Context, name string) (mattermost.User, error) {
	return mattermost.User{ID: "id-" + name, Username: name}, nil
}

func (c *fakeConn) User(_ context.Context, id string) (mattermost.User, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.users[id], nil
}

// CachedUser answers the event path without a round trip. The scripted users map
// stands in for the client's is_bot cache, so an author a test declared is known
// and any other author is a miss, exactly as an unwarmed cache behaves.
func (c *fakeConn) CachedUser(id string) (mattermost.User, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	user, known := c.users[id]

	return user, known
}

func (c *fakeConn) ChannelByName(_ context.Context, _, name string) (mattermost.Channel, error) {
	return c.channels[name], nil
}

func (c *fakeConn) Typing(_ context.Context, _, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.typing++

	return nil
}

func (c *fakeConn) AddReaction(_ context.Context, postID, emojiName string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.reactions = append(c.reactions, recordedReaction{PostID: postID, Emoji: emojiName})

	return c.reactErr
}

// Reactions returns a copy of every reaction the identity attempted, in order.
func (c *fakeConn) Reactions() []recordedReaction {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]recordedReaction(nil), c.reactions...)
}

// Posts returns a copy of everything the identity posted, in order.
func (c *fakeConn) Posts() []recordedPost {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]recordedPost(nil), c.posts...)
}

// TypingCount reports how many typing indicators were raised.
func (c *fakeConn) TypingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.typing
}

// push queues one event for the bridge's gate. The inbox is buffered, so a test
// may push before the identity has connected.
func (c *fakeConn) push(event *mattermost.Event) {
	c.inbox <- *event
}

// next reads one queued event or reports that ctx ended.
func (c *fakeConn) next(ctx context.Context) (mattermost.Event, bool) {
	select {
	case <-ctx.Done():
		return mattermost.Event{}, false
	case event := <-c.inbox:
		return event, true
	}
}

// dmEvent builds a direct-message event from a human.
func dmEvent(postID, text string) *mattermost.Event {
	return &mattermost.Event{
		Name:        mattermost.EventPosted,
		ChannelType: mattermost.ChannelDirect,
		SenderName:  humanName,
		Post: &mattermost.Post{
			ID: postID, UserID: humanUserID, ChannelID: channelID, Message: text,
		},
	}
}

// channelEvent builds a public-channel event. mentioned decides whether the
// server's own mention list names the bot, which is the first half of the
// addressed check.
func channelEvent(postID, rootID, text string, mentioned bool) *mattermost.Event {
	event := &mattermost.Event{
		Name:        mattermost.EventPosted,
		ChannelType: mattermost.ChannelOpen,
		SenderName:  humanName,
		Post: &mattermost.Post{
			ID: postID, UserID: humanUserID, ChannelID: channelID, RootID: rootID, Message: text,
		},
	}

	if mentioned {
		event.Mentions = []string{botUserID}
	}

	return event
}

// suite bundles the core and the fakes a case asserts against.
type suite struct {
	core    *bridge.Core
	chat    *bridgetest.Chat
	store   *bridgetest.Store
	metrics *bridgetest.Metrics
}

// newSuite builds a core over the shared fakes with a TTL long enough that no
// case races a proposal expiry.
func newSuite(t *testing.T) *suite {
	t.Helper()

	agent := bridgetest.NewChat()
	binder := bridgetest.NewStore()
	counters := bridgetest.NewMetrics()

	return &suite{
		core: bridge.NewCore(&bridge.CoreConfig{
			Chat:       agent,
			Store:      binder,
			Metrics:    counters,
			Logger:     quietLogger(),
			ConfirmTTL: time.Minute,
		}),
		chat:    agent,
		store:   binder,
		metrics: counters,
	}
}

// quietLogger keeps the adapter's warn lines out of the test output.
func quietLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// opsIdentity builds the standard single identity over conn.
func opsIdentity(conn mattermost.Conn, allowBotsIn ...string) mattermost.Identity {
	return mattermost.Identity{
		Client: conn,
		Name:   identityOps,
		Config: config.MattermostIdentity{
			Username:    botUsername,
			DisplayName: "Ops Agent",
			Workspace:   workspaceOps,
			Approvers:   []string{humanName},
			AllowBotsIn: allowBotsIn,
		},
	}
}

// startBridge runs the adapter until the case ends, draining the core on the way
// out so no conversation worker or proposal timer outlives it.
func startBridge(t *testing.T, unit *suite, loop config.BotLoop, identities ...mattermost.Identity) {
	t.Helper()

	startBridgeWithBackoff(t, unit, loop, time.Millisecond, identities...)
}

// startBridgeWithBackoff is startBridge with the reconnect delay spelled out,
// for the case that asserts on how the delay escalates.
func startBridgeWithBackoff(
	t *testing.T,
	unit *suite,
	loop config.BotLoop,
	backoff time.Duration,
	identities ...mattermost.Identity,
) {
	t.Helper()

	runBridge(t, unit, &mattermost.Config{
		Core:       unit.core,
		Metrics:    unit.metrics,
		Logger:     quietLogger(),
		Identities: identities,
		Server: config.Mattermost{
			URL: serverURL, Team: teamName, ConfirmTTL: 10 * time.Minute, BotLoop: loop,
		},
		BackoffBase: backoff,
	})
}

// startAckingBridge is startBridge with the acknowledgement reaction set, which
// the other cases leave empty so their fakes record no reactions.
func startAckingBridge(t *testing.T, unit *suite, emoji string, identities ...mattermost.Identity) {
	t.Helper()

	runBridge(t, unit, &mattermost.Config{
		Core:       unit.core,
		Metrics:    unit.metrics,
		Logger:     quietLogger(),
		Identities: identities,
		Server: config.Mattermost{
			URL: serverURL, Team: teamName, ConfirmTTL: 10 * time.Minute, AckReaction: emoji,
		},
		BackoffBase: time.Millisecond,
	})
}

// runBridge runs a built adapter until the case ends, draining the core on the
// way out so no conversation worker or proposal timer outlives it.
func runBridge(t *testing.T, unit *suite, cfg *mattermost.Config) {
	t.Helper()

	built := mattermost.New(cfg)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		built.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
		unit.core.Drain()
	})
}

// waitForPosts blocks until conn has recorded at least want posts.
func waitForPosts(t *testing.T, conn *fakeConn, want int) []recordedPost {
	t.Helper()

	bridgetest.WaitFor(t, func() bool { return len(conn.Posts()) >= want })

	return conn.Posts()
}

// TestDirectMessageRunsTurn proves a DM needs no mention, replies in the same
// channel with no thread root, and binds its session under the identity and
// channel (FR8).
func TestDirectMessageRunsTurn(t *testing.T) {
	t.Parallel()

	unit := newSuite(t)
	conn := newFakeConn(botUserID, botUsername)

	startBridge(t, unit, config.BotLoop{}, opsIdentity(conn))
	conn.push(dmEvent("p1", "hello there"))

	posts := waitForPosts(t, conn, 1)

	if posts[0].ChannelID != channelID || posts[0].RootID != "" {
		t.Fatalf("reply addressed to %+v, want channel %q with no root", posts[0], channelID)
	}

	if posts[0].Message != bridgetest.ReplyText {
		t.Fatalf("reply text = %q, want %q", posts[0].Message, bridgetest.ReplyText)
	}

	if !unit.chat.HasTurn("hello there") {
		t.Fatalf("agent turns = %+v, want the dm text", unit.chat.Turns())
	}

	if _, bound := unit.store.Binding(workspaceOps, "mattermost", identityOps+":"+channelID); !bound {
		t.Fatalf("no session bound under the dm chat key")
	}

	// The indicator is raised off the event socket's consumer goroutine, so it
	// can land after the reply it announced.
	bridgetest.WaitFor(t, func() bool { return conn.TypingCount() > 0 })
}

// TestChannelMentionRepliesInThread proves a root mention answers on its own
// thread and reaches the agent with the leading @username removed (FR9, FR10).
func TestChannelMentionRepliesInThread(t *testing.T) {
	t.Parallel()

	unit := newSuite(t)
	conn := newFakeConn(botUserID, botUsername)

	startBridge(t, unit, config.BotLoop{}, opsIdentity(conn))
	conn.push(channelEvent(rootPostID, "", "@cairn deploy please", true))

	posts := waitForPosts(t, conn, 1)

	if posts[0].RootID != rootPostID {
		t.Fatalf("reply root = %q, want the mentioned post id", posts[0].RootID)
	}

	if !unit.chat.HasTurn("deploy please") {
		t.Fatalf("agent turns = %+v, want the text with the mention stripped", unit.chat.Turns())
	}
}

// TestThreadContinuationKeepsSession proves a later mention in the same thread
// lands on the same conversation, which is what makes a thread one session
// rather than one per post (FR11).
func TestThreadContinuationKeepsSession(t *testing.T) {
	t.Parallel()

	unit := newSuite(t)
	conn := newFakeConn(botUserID, botUsername)

	startBridge(t, unit, config.BotLoop{}, opsIdentity(conn))
	conn.push(channelEvent(rootPostID, "", "@cairn first", true))
	waitForPosts(t, conn, 1)
	conn.push(channelEvent("p2", rootPostID, "@cairn second", true))
	waitForPosts(t, conn, 2)

	turns := unit.chat.Turns()
	if len(turns) != 2 {
		t.Fatalf("agent saw %d turns, want 2: %+v", len(turns), turns)
	}

	if turns[0].SessionID != turns[1].SessionID {
		t.Fatalf("thread turns landed on sessions %q and %q, want one session",
			turns[0].SessionID, turns[1].SessionID)
	}
}

// TestUnmentionedChannelPostIgnored proves channel chatter that does not name
// the identity never becomes a turn (FR9).
func TestUnmentionedChannelPostIgnored(t *testing.T) {
	t.Parallel()

	unit := newSuite(t)
	conn := newFakeConn(botUserID, botUsername)

	startBridge(t, unit, config.BotLoop{}, opsIdentity(conn))
	conn.push(channelEvent("p1", "", "just talking among ourselves", false))
	conn.push(dmEvent("p2", "now answer me"))

	// The DM is pushed second and answered, so a reply proves the channel post
	// was already through the gate and dropped rather than still in flight.
	waitForPosts(t, conn, 1)

	if unit.chat.HasTurn("just talking among ourselves") {
		t.Fatalf("unmentioned channel post became a turn: %+v", unit.chat.Turns())
	}
}

// TestMentionInsideCodeFenceIgnored proves a username quoted in a code sample
// does not wake the identity even when the server's mention list names it
// (FR10).
func TestMentionInsideCodeFenceIgnored(t *testing.T) {
	t.Parallel()

	unit := newSuite(t)
	conn := newFakeConn(botUserID, botUsername)

	startBridge(t, unit, config.BotLoop{}, opsIdentity(conn))
	conn.push(channelEvent("p1", "", "see ```@cairn help``` in the docs", true))
	conn.push(dmEvent("p2", "now answer me"))

	waitForPosts(t, conn, 1)

	if unit.chat.HasTurn("see ```@cairn help``` in the docs") {
		t.Fatalf("a fenced mention became a turn: %+v", unit.chat.Turns())
	}
}

// botEvent builds a channel mention authored by something automated, with the
// props the server stamps.
func botEvent(postID string, props map[string]string) *mattermost.Event {
	event := channelEvent(postID, "", "@cairn ping", true)
	event.Post.UserID = "bot-other"
	event.Post.Props = props
	event.SenderName = "otherbot"

	return event
}

// TestBotPostNeedsOptIn proves provenance blocks automated traffic by default
// and that either opt-in admits it: the channel listed under allow_bots_in, or
// the post's own activate_ai prop (FR14).
func TestBotPostNeedsOptIn(t *testing.T) {
	t.Parallel()

	cases := []struct {
		props       map[string]string
		name        string
		allowBotsIn []string
		wantAnswer  bool
	}{
		{name: "no opt-in drops", props: map[string]string{propFromBot: propTrue}, wantAnswer: false},
		{
			name:  "allow_bots_in by channel id admits",
			props: map[string]string{propFromBot: propTrue}, allowBotsIn: []string{channelID}, wantAnswer: true,
		},
		{
			name:  "activate_ai prop admits",
			props: map[string]string{propFromWebhook: propTrue, propActivateAI: propTrue}, wantAnswer: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			unit := newSuite(t)
			conn := newFakeConn(botUserID, botUsername)

			startBridge(t, unit, config.BotLoop{}, opsIdentity(conn, testCase.allowBotsIn...))
			conn.push(botEvent("p1", testCase.props))
			conn.push(dmEvent("p2", "now answer me"))

			want := 1
			if testCase.wantAnswer {
				want = 2
			}

			waitForPosts(t, conn, want)

			if got := unit.chat.HasTurn("ping"); got != testCase.wantAnswer {
				t.Fatalf("bot post answered = %v, want %v (turns %+v)", got, testCase.wantAnswer, unit.chat.Turns())
			}
		})
	}
}

// TestAllowBotsInResolvesChannelName proves a #name entry is resolved against
// the configured team at connect, since the event stream only carries ids.
func TestAllowBotsInResolvesChannelName(t *testing.T) {
	t.Parallel()

	unit := newSuite(t)
	conn := newFakeConn(botUserID, botUsername)
	conn.channels[channelName] = mattermost.Channel{ID: channelID, Name: channelName, Type: mattermost.ChannelOpen}

	startBridge(t, unit, config.BotLoop{}, opsIdentity(conn, "#"+channelName))
	conn.push(botEvent("p1", map[string]string{propFromBot: propTrue}))

	waitForPosts(t, conn, 1)

	if !unit.chat.HasTurn("ping") {
		t.Fatalf("bot post in a named allow_bots_in channel was dropped: %+v", unit.chat.Turns())
	}
}

// TestLoopLimiterStopsRunawayBots proves the window closes a conversation the
// provenance opt-in deliberately opened: the post that would exceed max_events
// trips the cooldown and is counted unauthorized (FR15).
func TestLoopLimiterStopsRunawayBots(t *testing.T) {
	t.Parallel()

	unit := newSuite(t)
	conn := newFakeConn(botUserID, botUsername)

	loop := config.BotLoop{MaxEvents: 2, Window: time.Minute, Cooldown: time.Minute}
	startBridge(t, unit, loop, opsIdentity(conn, channelID))

	first := botEvent(rootPostID, map[string]string{propFromBot: propTrue})
	conn.push(first)
	waitForPosts(t, conn, 1)

	for _, postID := range []string{"p2", "p3"} {
		event := botEvent(postID, map[string]string{propFromBot: propTrue})
		event.Post.RootID = rootPostID
		conn.push(event)
	}

	waitForPosts(t, conn, 2)
	bridgetest.WaitFor(t, func() bool {
		return unit.metrics.UpdateCount("mattermost", identityOps, bridge.OutcomeUnauthorized) == 1
	})

	if got := len(conn.Posts()); got != 2 {
		t.Fatalf("identity posted %d replies, want 2 before the limiter tripped", got)
	}
}

// TestSilentReplyPostsNothing proves the core's [SILENT] contract reaches this
// transport: the turn runs and nothing is posted (FR16).
func TestSilentReplyPostsNothing(t *testing.T) {
	t.Parallel()

	unit := newSuite(t)
	conn := newFakeConn(botUserID, botUsername)

	unit.chat.SetReply("[SILENT]")

	startBridge(t, unit, config.BotLoop{}, opsIdentity(conn))
	conn.push(dmEvent("p1", "stay quiet"))

	bridgetest.WaitFor(t, func() bool { return unit.chat.HasTurn("stay quiet") })

	if got := conn.Posts(); len(got) != 0 {
		t.Fatalf("silent reply posted %+v, want nothing", got)
	}
}

// TestOwnPostDropped proves the identity does not answer its own echo, which is
// what stops a bot talking to itself forever (FR13).
func TestOwnPostDropped(t *testing.T) {
	t.Parallel()

	unit := newSuite(t)
	conn := newFakeConn(botUserID, botUsername)

	startBridge(t, unit, config.BotLoop{}, opsIdentity(conn))

	echo := dmEvent("p1", "acknowledged")
	echo.Post.UserID = botUserID
	conn.push(echo)
	conn.push(dmEvent("p2", "now answer me"))

	waitForPosts(t, conn, 1)

	if unit.chat.HasTurn("acknowledged") {
		t.Fatalf("the identity answered its own post: %+v", unit.chat.Turns())
	}
}

// TestEachIdentityAnswersOnlyItsOwnMention proves two agents on one serve stay
// in their own lane: a post naming one of them is a turn for that one and
// nothing at all for the other.
func TestEachIdentityAnswersOnlyItsOwnMention(t *testing.T) {
	t.Parallel()

	unit := newSuite(t)

	alphaConn := newFakeConn("bot-alpha", "alpha")
	betaConn := newFakeConn("bot-beta", "beta")

	alpha := mattermost.Identity{
		Client: alphaConn, Name: "alpha",
		Config: config.MattermostIdentity{Username: "alpha", Workspace: workspaceOps},
	}
	beta := mattermost.Identity{
		Client: betaConn, Name: "beta",
		Config: config.MattermostIdentity{Username: "beta", Workspace: workspaceOps},
	}

	startBridge(t, unit, config.BotLoop{}, alpha, beta)

	// Both identities see the same channel post; only alpha is in its mention
	// list, so only alpha may answer.
	forAlpha := channelEvent(rootPostID, "", "@alpha status please", false)
	forAlpha.Mentions = []string{"bot-alpha"}

	alphaConn.push(forAlpha)
	betaConn.push(forAlpha)

	posts := waitForPosts(t, alphaConn, 1)

	if posts[0].Message != bridgetest.ReplyText {
		t.Fatalf("alpha replied %q, want %q", posts[0].Message, bridgetest.ReplyText)
	}

	if got := betaConn.Posts(); len(got) != 0 {
		t.Fatalf("beta answered a post naming alpha: %+v", got)
	}
}

// TestUsernameMismatchRefusesToAnswer proves the FR5 cross-check: a token whose
// account is not the configured username never reaches the gate, so a
// misconfigured identity stays silent instead of answering as the wrong bot. A
// second, correctly configured identity gets the same DM and its reply is the
// signal that the mismatched one has already had its chance.
func TestUsernameMismatchRefusesToAnswer(t *testing.T) {
	t.Parallel()

	unit := newSuite(t)
	wrongConn := newFakeConn("bot-wrong", "someone-else")
	rightConn := newFakeConn(botUserID, botUsername)

	wrong := mattermost.Identity{
		Client: wrongConn, Name: "wrong",
		Config: config.MattermostIdentity{Username: botUsername, Workspace: workspaceOps},
	}

	startBridge(t, unit, config.BotLoop{}, wrong, opsIdentity(rightConn))

	wrongConn.push(dmEvent("p1", "hello there"))
	rightConn.push(dmEvent("p2", "hello there"))

	waitForPosts(t, rightConn, 1)

	if got := wrongConn.Posts(); len(got) != 0 {
		t.Fatalf("a mismatched identity posted %+v, want nothing", got)
	}
}

// dialRefusingConn accepts the REST connect and refuses the WebSocket upgrade,
// which is what a reverse proxy passing REST while blocking the upgrade looks
// like from the bridge's side.
type dialRefusingConn struct {
	*fakeConn

	connects atomic.Int32
}

func (c *dialRefusingConn) Connect(ctx context.Context) error {
	c.connects.Add(1)

	return c.fakeConn.Connect(ctx)
}

func (*dialRefusingConn) Listen(_ context.Context, _ chan<- mattermost.Event) error {
	return &mattermost.APIError{
		Class:   mattermost.ClassTransient,
		Method:  "Listen",
		Message: "dial: upgrade refused",
		Cause:   fmt.Errorf("%w: upgrade refused", mattermost.ErrSocketDial),
	}
}

// TestBackoffEscalatesWhenTheUpgradeIsRefused proves a REST connect that
// succeeds while the WebSocket upgrade fails does not reset the reconnect delay.
// Resetting it leaves the identity redialing once per base delay forever instead
// of escalating toward the one-minute ceiling (FR7).
func TestBackoffEscalatesWhenTheUpgradeIsRefused(t *testing.T) {
	t.Parallel()

	const base = 40 * time.Millisecond

	unit := newSuite(t)
	conn := &dialRefusingConn{fakeConn: newFakeConn(botUserID, botUsername)}

	started := time.Now()

	startBridgeWithBackoff(t, unit, config.BotLoop{}, base, opsIdentity(conn))

	bridgetest.WaitFor(t, func() bool { return conn.connects.Load() >= 4 })

	// Escalating puts the fourth connect at 40+80+160ms. Resetting every cycle
	// would have reached it at 120ms, so anything under 200ms means no escalation.
	if elapsed := time.Since(started); elapsed < 5*base {
		t.Fatalf("four connects took %v, want at least %v: the backoff reset on a refused upgrade", elapsed, 5*base)
	}
}

// TestBareMentionRunsNoTurn proves a mention with nothing after it is dropped
// rather than billed as a driver call against an empty message. The adapter's
// blank-message guard runs before the mention is stripped, so the emptiness only
// shows up afterwards (edge case 9).
func TestBareMentionRunsNoTurn(t *testing.T) {
	t.Parallel()

	unit := newSuite(t)
	conn := newFakeConn(botUserID, botUsername)

	startBridge(t, unit, config.BotLoop{}, opsIdentity(conn))
	conn.push(channelEvent(rootPostID, "", "@cairn", true))
	conn.push(dmEvent("p2", "now answer me"))

	waitForPosts(t, conn, 1)

	if turns := unit.chat.Turns(); len(turns) != 1 || turns[0].Message != "now answer me" {
		t.Fatalf("agent turns = %+v, want only the dm that carried text", turns)
	}
}

// TestNewSubstitutesMissingMetrics proves a bridge wired without a recorder
// refuses a post instead of panicking on the first counter call, which would
// otherwise crash the identity loop on every refused bot post.
func TestNewSubstitutesMissingMetrics(t *testing.T) {
	t.Parallel()

	unit := newSuite(t)
	conn := newFakeConn(botUserID, botUsername)

	built := mattermost.New(&mattermost.Config{
		Core:       unit.core,
		Logger:     quietLogger(),
		Identities: []mattermost.Identity{opsIdentity(conn)},
		Server:     config.Mattermost{URL: serverURL, Team: teamName, ConfirmTTL: time.Minute},
	})

	if built == nil {
		t.Fatal("New returned no bridge")
	}

	if len(built.Senders()) != 1 {
		t.Fatalf("Senders() = %d identities, want 1", len(built.Senders()))
	}
}

// ackEmoji is the reaction the acknowledgement cases configure. It differs from
// the config default so a case proves the configured value reaches the server.
const ackEmoji = "white_check_mark"

// TestAcceptedPostGetsAckReaction proves an accepted post carries the configured
// reaction while its turn is still gated, so the sender sees it was picked up
// before any reply exists.
func TestAcceptedPostGetsAckReaction(t *testing.T) {
	t.Parallel()

	cases := []struct {
		event  *mattermost.Event
		name   string
		text   string
		postID string
	}{
		{
			name: "channel mention", event: channelEvent(rootPostID, "", "@cairn deploy please", true),
			text: "deploy please", postID: rootPostID,
		},
		{name: "direct message", event: dmEvent("dm-1", "hello there"), text: "hello there", postID: "dm-1"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			unit := newSuite(t)
			unit.chat.GateOn(testCase.text)

			conn := newFakeConn(botUserID, botUsername)

			startAckingBridge(t, unit, ackEmoji, opsIdentity(conn))
			conn.push(testCase.event)

			bridgetest.WaitFor(t, func() bool { return len(conn.Reactions()) > 0 })

			if posts := conn.Posts(); len(posts) != 0 {
				t.Fatalf("identity replied %+v while the turn was still gated", posts)
			}

			want := []recordedReaction{{PostID: testCase.postID, Emoji: ackEmoji}}
			if got := conn.Reactions(); !slices.Equal(got, want) {
				t.Fatalf("reactions = %+v, want %+v", got, want)
			}

			unit.chat.Release()
			waitForPosts(t, conn, 1)
		})
	}
}

// TestUnprocessedPostGetsNoAckReaction proves only a post that becomes a turn is
// acknowledged, and that an empty ack_reaction turns the reaction off. The DM
// pushed second is answered, so the first post was already through the gate.
func TestUnprocessedPostGetsNoAckReaction(t *testing.T) {
	t.Parallel()

	answered := recordedReaction{PostID: "p2", Emoji: ackEmoji}

	cases := []struct {
		first       *mattermost.Event
		name        string
		emoji       string
		want        []recordedReaction
		wantReplies int
	}{
		{
			name:  "automated post outside allow_bots_in",
			first: botEvent("p1", map[string]string{propFromBot: propTrue}),
			emoji: ackEmoji, want: []recordedReaction{answered}, wantReplies: 1,
		},
		{
			name:  "bare mention with nothing to answer",
			first: channelEvent("p1", "", "@cairn", true),
			emoji: ackEmoji, want: []recordedReaction{answered}, wantReplies: 1,
		},
		{
			name:  "reaction turned off",
			first: channelEvent("p1", "", "@cairn deploy please", true),
			emoji: "", want: nil, wantReplies: 2,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			unit := newSuite(t)
			conn := newFakeConn(botUserID, botUsername)

			startAckingBridge(t, unit, testCase.emoji, opsIdentity(conn))
			conn.push(testCase.first)
			conn.push(dmEvent("p2", "now answer me"))

			waitForPosts(t, conn, testCase.wantReplies)
			bridgetest.WaitFor(t, func() bool { return len(conn.Reactions()) >= len(testCase.want) })

			if got := conn.Reactions(); !slices.Equal(got, testCase.want) {
				t.Fatalf("reactions = %+v, want %+v", got, testCase.want)
			}
		})
	}
}

// TestAckReactionFailureStillRunsTurn proves a refused reaction costs only the
// decoration: the turn still runs and the reply still lands.
func TestAckReactionFailureStillRunsTurn(t *testing.T) {
	t.Parallel()

	unit := newSuite(t)
	conn := newFakeConn(botUserID, botUsername)
	conn.reactErr = &mattermost.APIError{Class: mattermost.ClassPermanent, Method: "AddReaction", Status: 403}

	startAckingBridge(t, unit, ackEmoji, opsIdentity(conn))
	conn.push(dmEvent("p1", "hello there"))

	waitForPosts(t, conn, 1)
	bridgetest.WaitFor(t, func() bool { return len(conn.Reactions()) == 1 })

	if !unit.chat.HasTurn("hello there") {
		t.Fatalf("agent turns = %+v, want the dm text despite the failed reaction", unit.chat.Turns())
	}
}
