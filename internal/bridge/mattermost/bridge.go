package mattermost

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/chadit/CairnOS/internal/bridge"
	"github.com/chadit/CairnOS/internal/config"
)

const (
	// transportName is the bridge_sessions transport column value and the label
	// every counter this adapter records carries.
	transportName = "mattermost"

	// chatKeySep joins the parts of a conversation key: identity and channel for
	// a DM, plus the thread root for a channel mention (FR8, FR11). It is stored
	// in bridge_sessions, so it is part of the on-disk shape.
	chatKeySep = ":"

	// channelNameSigil marks an allow_bots_in entry written as a name rather than
	// a raw channel id.
	channelNameSigil = "#"

	// eventBuffer decouples the socket reader from the gate, so a slow lookup on
	// one post does not stall the read that would have delivered the next.
	eventBuffer = 32

	// resultBufferSize gives Listen's terminal error a slot, so its goroutine
	// exits even after pump has returned on a canceled context.
	resultBufferSize = 1
)

// Identity is one configured agent as serve resolved it: the config block, the
// name its token env and its metrics label derive from, and the client built
// for that identity's token.
type Identity struct {
	Client Conn
	Name   string
	Config config.MattermostIdentity
}

// Config wires one Bridge.
type Config struct {
	// Core is the transport-neutral half every admitted post is handed to. Serve
	// builds it once and shares it with the Telegram adapter, so both transports
	// see one session map and one set of pending proposals.
	Core *bridge.Core

	// Metrics counts the posts this adapter refuses before the core sees them. A
	// nil value is replaced with a no-op recorder: a wiring change that drops the
	// adapter should cost counters, not panic on the first refused post.
	Metrics bridge.Metrics

	// Logger receives the adapter's structured logs; nil falls back to the slog
	// default.
	Logger *slog.Logger

	// Identities are the agents to connect, one socket and one goroutine each.
	Identities []Identity

	// Server is the shared server block: the URL each identity advertises on its
	// messaging description, the team #channel references resolve against, the
	// confirm TTL the proposal line names, and the bot-loop window.
	Server config.Mattermost

	// BackoffBase is the first reconnect delay; each failed cycle doubles it up
	// to a one-minute ceiling. Zero takes bridge.DefaultBackoffBase. Tests
	// shorten it so reconnect assertions stay fast.
	BackoffBase time.Duration

	// ChannelCacheTTL bounds how long a resolved send target keeps pointing at
	// one channel id; zero takes channelCacheTTL. Tests shorten it so a
	// re-resolution assertion does not wait out the real window.
	ChannelCacheTTL time.Duration
}

// Bridge holds one WebSocket connection per configured identity and hands every
// post that clears the gate to the shared core. Run owns those connections; it
// is not safe to call twice.
type Bridge struct {
	core *bridge.Core
	log  *slog.Logger

	// selfIDs holds every identity's own Mattermost user id on this serve. The
	// own-post filter spans all of them, not just the one that received the
	// event, so two agents in one channel never answer each other's replies
	// (FR13). Each identity's loop writes its id at connect while the others
	// read, so the set is guarded.
	selfIDs map[string]struct{}

	metrics    bridge.Metrics
	identities []*identity

	selfMu sync.RWMutex

	backoffBase time.Duration
}

// identity is one agent's runtime state. Every field but channels is touched
// only by that identity's own loop goroutine, the single consumer of its event
// socket, so those need no lock. The send path reaches the same value from API
// goroutines, and channels carries its own guard for them.
type identity struct {
	client Conn
	out    *outbound

	// limiter is the bot-to-bot window, keyed by conversation (FR15).
	limiter *LoopLimiter

	// approvers holds the usernames allowed to resolve a proposal, folded to
	// lowercase because Mattermost usernames are case-insensitive.
	approvers map[string]struct{}

	// allowBots holds the channel ids where an automated post still earns a
	// reply. It is rebuilt after every connect, because a #name entry resolves
	// to an id the event stream never spells out.
	allowBots map[string]struct{}

	// tripped remembers which conversations are already serving a cooldown, so a
	// channel that keeps posting writes one warn line rather than one per post.
	tripped map[string]bool

	// channels caches the send path's target-to-channel resolutions.
	channels *channelCache

	name string

	// serverURL and team are the shared server block's values, copied here so the
	// send path works through the registry without reaching back to the Bridge.
	serverURL string
	team      string

	// ackReaction is the emoji put on each accepted post; empty skips it.
	ackReaction string

	cfg config.MattermostIdentity
}

// New builds the bridge over the clients serve already created, one per
// identity. It opens no connection: Run owns the socket lifetime, so a
// Mattermost server that is down at startup delays no listener binding.
func New(cfg *Config) *Bridge {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	backoff := cfg.BackoffBase
	if backoff <= 0 {
		backoff = bridge.DefaultBackoffBase
	}

	metrics := cfg.Metrics
	if metrics == nil {
		metrics = noopMetrics{}
	}

	built := &Bridge{
		core:        cfg.Core,
		metrics:     metrics,
		log:         log,
		identities:  make([]*identity, 0, len(cfg.Identities)),
		selfIDs:     map[string]struct{}{},
		backoffBase: backoff,
	}

	for index := range cfg.Identities {
		built.identities = append(built.identities, newIdentity(&cfg.Identities[index], cfg))
	}

	return built
}

// noopMetrics stands in for a recorder the wiring did not pass. The field
// comment used to say nil was not allowed and nothing checked it, so the first
// refused post panicked inside guard and the identity reconnected and crashed
// again on the next one, naming the recover site rather than the missing
// dependency.
type noopMetrics struct{}

func (noopMetrics) Update(_, _, _ string)    {}
func (noopMetrics) Turn(_, _ string, _ bool) {}
func (noopMetrics) Confirm(_, _, _ string)   {}
func (noopMetrics) SendFailure(_, _ string)  {}

// newIdentity assembles one agent's runtime state: its send surface, its
// approver set, and the loop window it shares across every conversation it
// answers.
func newIdentity(declared *Identity, cfg *Config) *identity {
	approvers := make(map[string]struct{}, len(declared.Config.Approvers))
	for _, name := range declared.Config.Approvers {
		approvers[strings.ToLower(name)] = struct{}{}
	}

	return &identity{
		client: declared.Client,
		out:    &outbound{Conn: declared.Client, confirmTTL: cfg.Server.ConfirmTTL},
		limiter: NewLoopLimiter(LoopLimitConfig{
			Window:    cfg.Server.BotLoop.Window,
			Cooldown:  cfg.Server.BotLoop.Cooldown,
			MaxEvents: cfg.Server.BotLoop.MaxEvents,
		}),
		approvers:   approvers,
		allowBots:   map[string]struct{}{},
		tripped:     map[string]bool{},
		channels:    newChannelCache(cfg.ChannelCacheTTL),
		cfg:         declared.Config,
		name:        declared.Name,
		serverURL:   cfg.Server.URL,
		team:        cfg.Server.Team,
		ackReaction: cfg.Server.AckReaction,
	}
}

// Run connects every identity and holds its event socket until ctx ends. Each
// identity owns one goroutine, so a refused token or a panic in one gate leaves
// the others answering (NFR3). Draining the core is the caller's job: two
// transports share one core, and the first Run to return would otherwise drain
// the other transport's conversation workers.
func (b *Bridge) Run(ctx context.Context) {
	var running sync.WaitGroup

	for _, agent := range b.identities {
		running.Go(func() { b.runIdentity(ctx, agent) })
	}

	running.Wait()
}

// runIdentity reconnects one identity for as long as ctx lives, doubling the
// delay after each failed cycle up to the one-minute ceiling and resetting it
// once the event socket actually ran (FR7).
func (b *Bridge) runIdentity(ctx context.Context, agent *identity) {
	delay := b.backoffBase

	for ctx.Err() == nil {
		if b.cycle(ctx, agent) {
			delay = b.backoffBase
		}

		if !bridge.Sleep(ctx, delay) {
			return
		}

		delay = bridge.NextBackoff(delay)
	}
}

// cycle runs one connect-and-listen pass and reports whether the event socket
// ran, which is what resets the caller's backoff. A REST connect that succeeds
// while the upgrade is refused does not count. A panic leaves the result false,
// so a crashing identity backs off rather than spinning.
func (b *Bridge) cycle(ctx context.Context, agent *identity) bool {
	var connected bool

	b.guard(ctx, agent, func() { connected = b.connectAndListen(ctx, agent) })

	return connected
}

// guard runs one identity's work with a panic recovered and logged, so one
// malformed post cannot take the identity, or its siblings, down (NFR3).
func (b *Bridge) guard(ctx context.Context, agent *identity, work func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			b.log.ErrorContext(ctx, "mattermost identity loop crashed",
				"identity", agent.name, "panic", recovered)
		}
	}()

	work()
}

// connectAndListen learns the identity, cross-checks the username against the
// account the token really belongs to (FR5), then pumps posted events through
// the gate until the socket fails or ctx ends. A mismatch reports not-connected
// so the caller backs off instead of answering as the wrong account.
func (b *Bridge) connectAndListen(ctx context.Context, agent *identity) bool {
	if err := agent.client.Connect(ctx); err != nil {
		b.log.WarnContext(ctx, "mattermost connect failed", "identity", agent.name, "error", err)

		return false
	}

	account := agent.client.Me()
	if !strings.EqualFold(account.Username, agent.cfg.Username) {
		b.log.ErrorContext(ctx, "mattermost identity username does not match its token",
			"identity", agent.name, "configured", agent.cfg.Username, "token_user", account.Username)

		return false
	}

	b.noteSelf(account.ID)
	b.resolveAllowBots(ctx, agent)

	return b.pump(ctx, agent)
}

// pump forwards posted events to the gate until the socket read fails or ctx
// ends, and reports whether the socket ever carried anything. Listen blocks
// writing into the channel, so it runs on its own goroutine and this loop is its
// only consumer.
func (b *Bridge) pump(ctx context.Context, agent *identity) bool {
	listenCtx, stop := context.WithCancel(ctx)
	defer stop()

	events := make(chan Event, eventBuffer)
	closed := make(chan error, resultBufferSize)

	go func() { closed <- agent.client.Listen(listenCtx, events) }()

	for {
		select {
		case <-ctx.Done():
			return true
		case cause := <-closed:
			b.noteSocketClosed(ctx, agent, cause)

			// A refused upgrade means the socket carried nothing, so the caller
			// keeps escalating instead of redialing once a second forever (FR7).
			return !errors.Is(cause, ErrSocketDial)
		case event := <-events:
			b.dispatch(ctx, agent, &event)
		}
	}
}

// noteSocketClosed logs a dropped event socket once per cycle. A socket closing
// during shutdown is expected, so a canceled ctx stays quiet.
func (b *Bridge) noteSocketClosed(ctx context.Context, agent *identity, cause error) {
	if ctx.Err() != nil {
		return
	}

	b.log.WarnContext(ctx, "mattermost event socket closed", "identity", agent.name, "error", cause)
}

// dispatch applies the inbound gate to one posted event and hands what survives
// to the core. The order is design 3.3: this serve's own posts, then system and
// empty posts, then whether the post addresses this identity at all, then the
// two loop defenses, and only then the turn.
func (b *Bridge) dispatch(ctx context.Context, agent *identity, event *Event) {
	post := event.Post

	if b.isSelf(post.UserID) {
		return
	}

	// A system post (a join, a rename) has a non-empty type and nothing a reader
	// addressed to anyone; an empty message has no turn in it.
	if post.Type != "" || strings.TrimSpace(post.Message) == "" {
		return
	}

	direct := event.ChannelType == ChannelDirect
	if !direct && !addressed(agent, event) {
		return
	}

	turn := newRouted(agent.name, event, direct)
	turn.automated = b.automated(ctx, agent, post)

	if !b.admit(ctx, agent, turn) {
		return
	}

	b.handle(ctx, agent, turn)
}

// routed is one post the DM and mention checks already accepted, plus the
// addressing the core needs. ChatKey and Thread are the FR8 and FR11 mapping: a
// DM is one conversation per channel, a channel mention is one per thread root.
type routed struct {
	event   *Event
	chatKey string
	thread  string

	// automated records what provenance decided, so the gate and the inbound's
	// Sender agree without a second author lookup.
	automated bool
}

// newRouted resolves a post's conversation key and reply root. A DM answers in
// the channel with no thread; a channel mention answers on the post's thread
// root, which is the post itself when it opens the thread (FR9, FR11).
func newRouted(identityName string, event *Event, direct bool) *routed {
	post := event.Post

	if direct {
		return &routed{event: event, chatKey: identityName + chatKeySep + post.ChannelID}
	}

	root := post.RootID
	if root == "" {
		root = post.ID
	}

	return &routed{
		event:   event,
		chatKey: identityName + chatKeySep + post.ChannelID + chatKeySep + root,
		thread:  root,
	}
}

// addressed reports whether a channel post names this identity. The server's
// own mention list says it was mentioned; the text check says the mention is
// prose rather than a username quoted in a code sample (FR9, FR10).
func addressed(agent *identity, event *Event) bool {
	account := agent.client.Me()
	if !slices.Contains(event.Mentions, account.ID) {
		return false
	}

	return MentionsInText(event.Post.Message, account.Username)
}

// admit applies the two loop defenses to a post already addressed to the
// identity: provenance decides whether an automated poster may speak here at
// all, and the window catches the bot-to-bot traffic allow_bots_in deliberately
// lets through (design D6). A human post clears both untouched.
func (b *Bridge) admit(ctx context.Context, agent *identity, turn *routed) bool {
	if !turn.automated {
		return true
	}

	post := turn.event.Post
	if !agent.admitsBot(post.ChannelID) && !ActivateAI(post) {
		b.refuse(ctx, agent, "automated post outside allow_bots_in", turn.chatKey)

		return false
	}

	if !agent.limiter.Admit(turn.chatKey, time.Now()) {
		agent.noteLoopTrip(ctx, b.log, turn.chatKey)
		b.metrics.Update(transportName, agent.name, bridge.OutcomeUnauthorized)

		return false
	}

	agent.clearLoopTrip(turn.chatKey)

	return true
}

// refuse counts and logs one post the bot gate turned away. It is counted
// unauthorized because the post was addressed to this identity and the operator
// set the rule that declined it, which is the same shape as a Telegram sender
// off the allowlist (FR14).
func (b *Bridge) refuse(ctx context.Context, agent *identity, reason, key string) {
	b.metrics.Update(transportName, agent.name, bridge.OutcomeUnauthorized)
	b.log.DebugContext(ctx, "mattermost post refused",
		"identity", agent.name, "chat_key", key, "reason", reason)
}

// handle turns an admitted post into the core's inbound, reacting to it and
// raising the typing indicator on the way, so the conversation shows the agent
// picked the post up before the turn finishes (FR17).
func (b *Bridge) handle(ctx context.Context, agent *identity, turn *routed) {
	post := turn.event.Post

	// dispatch's blank-message guard runs on the raw message. Someone typing
	// "@cairn" alone to get attention leaves nothing behind once the mention is
	// stripped, and an empty turn still bills a driver call (edge case 9).
	text := StripMention(post.Message, agent.client.Me().Username)
	if strings.TrimSpace(text) == "" {
		return
	}

	go b.acknowledge(ctx, agent, post.ID)

	sender := b.senderName(ctx, agent, post, turn.event.SenderName)

	addr := bridge.Address{
		Transport: transportName,
		Identity:  agent.name,
		ChatKey:   turn.chatKey,
		Channel:   post.ChannelID,
		Thread:    turn.thread,
	}

	// The indicator costs a POST bounded only by the client timeout, and pump is
	// the socket's only consumer, so raising it inline stalls the read that would
	// have delivered the next post. It is decoration either way (FR17, NFR2).
	go b.typing(ctx, agent, post.ChannelID, turn.thread)

	b.core.Handle(ctx, &bridge.Inbound{
		Addr:   addr,
		Sender: bridge.Sender{ID: post.UserID, Name: sender, IsBot: turn.automated},
		Grant: bridge.Grant{
			Workspace:    agent.cfg.Workspace,
			Personality:  agent.cfg.Personality,
			AllowConfirm: agent.approves(sender),
		},
		Text: text,
		Out:  agent.out,
		Kind: bridge.KindMessage,
	})
}

// acknowledge reacts to an accepted post off the socket's consumer, like typing,
// so a slow POST never stalls the read (NFR2); a failure is logged, never fatal.
func (b *Bridge) acknowledge(ctx context.Context, agent *identity, postID string) {
	if agent.ackReaction == "" {
		return
	}

	if err := agent.client.AddReaction(ctx, postID, agent.ackReaction); err != nil {
		b.log.DebugContext(ctx, "mattermost ack reaction failed",
			"identity", agent.name, "post_id", postID, "emoji", agent.ackReaction, "error", err)
	}
}

// typing raises the typing indicator for one turn off the socket's consumer,
// like acknowledge. A turn that runs without the indicator is still a turn, so a
// failure is logged at debug and never costs the reply (FR17).
func (b *Bridge) typing(ctx context.Context, agent *identity, channelID, threadID string) {
	if err := agent.client.Typing(ctx, channelID, threadID); err != nil {
		b.log.DebugContext(ctx, "mattermost typing indicator failed",
			"identity", agent.name, "channel_id", channelID, "error", err)
	}
}

// automated reports whether a post came from something other than a person
// typing. The props answer without a round trip for every bot account and every
// integration the server stamps, so only a post carrying none of them consults
// the author cache at all (FR14).
func (b *Bridge) automated(ctx context.Context, agent *identity, post *Post) bool {
	if IsAutomated(post, nil) {
		return true
	}

	author, known := b.author(ctx, agent, post.UserID)
	if !known {
		return false
	}

	return IsAutomated(post, &author)
}

// senderName is the username the approver check runs against. The event
// normally carries it; a frame without one falls back to the author cache, so a
// missing wire field does not quietly demote an approver.
func (b *Bridge) senderName(ctx context.Context, agent *identity, post *Post, carried string) string {
	if carried != "" {
		return carried
	}

	author, known := b.author(ctx, agent, post.UserID)
	if !known {
		return ""
	}

	return author.Username
}

// author reads a post's author out of the client's is_bot cache. pump is the
// event socket's only consumer, so a round trip here stalls the read that would
// have delivered the next post and eventually makes the server close the socket
// (NFR2). A miss warms the cache elsewhere and answers unknown for this post.
func (b *Bridge) author(ctx context.Context, agent *identity, userID string) (User, bool) {
	if cached, known := agent.client.CachedUser(userID); known {
		return cached, true
	}

	go b.warmAuthor(ctx, agent, userID)

	return User{}, false
}

// warmAuthor fills the author cache off the consumer goroutine. is_bot and the
// username do not change over a process lifetime, so the next post from the same
// author is decided without a wait.
func (b *Bridge) warmAuthor(ctx context.Context, agent *identity, userID string) {
	if _, err := agent.client.User(ctx, userID); err != nil {
		b.log.DebugContext(ctx, "mattermost author lookup failed",
			"identity", agent.name, "user_id", userID, "error", err)
	}
}

// noteSelf records one identity's own user id so every identity's gate drops
// this serve's posts, not only the ones it wrote itself (FR13).
func (b *Bridge) noteSelf(userID string) {
	b.selfMu.Lock()
	defer b.selfMu.Unlock()

	b.selfIDs[userID] = struct{}{}
}

// isSelf reports whether a post's author is one of this serve's own identities.
func (b *Bridge) isSelf(userID string) bool {
	b.selfMu.RLock()
	defer b.selfMu.RUnlock()

	_, mine := b.selfIDs[userID]

	return mine
}

// resolveAllowBots rebuilds the admitted-channel set after a connect: a raw id
// passes through, and a #name is resolved against the configured team because
// the event stream only ever carries channel ids. A name that will not resolve
// is logged and left out rather than failing the connect, so one stale entry
// cannot silence the identity.
func (b *Bridge) resolveAllowBots(ctx context.Context, agent *identity) {
	admitted := make(map[string]struct{}, len(agent.cfg.AllowBotsIn))

	for _, entry := range agent.cfg.AllowBotsIn {
		channelID, resolved := b.resolveChannelRef(ctx, agent, entry)
		if resolved {
			admitted[channelID] = struct{}{}
		}
	}

	agent.allowBots = admitted
}

// resolveChannelRef turns one allow_bots_in entry into a channel id.
func (b *Bridge) resolveChannelRef(ctx context.Context, agent *identity, entry string) (string, bool) {
	name, named := strings.CutPrefix(entry, channelNameSigil)
	if !named {
		return entry, true
	}

	channel, err := agent.client.ChannelByName(ctx, agent.team, name)
	if err != nil {
		b.log.WarnContext(ctx, "mattermost allow_bots_in channel not resolved",
			"identity", agent.name, "channel", entry, "error", err)

		return "", false
	}

	return channel.ID, true
}

// admitsBot reports whether an automated post in this channel still earns a
// reply.
func (i *identity) admitsBot(channelID string) bool {
	_, allowed := i.allowBots[channelID]

	return allowed
}

// approves reports whether a sender may resolve a proposal. An empty username
// never matches, so a post whose author could not be named leaves the proposal
// to expire rather than handing the decision to an unidentified account.
func (i *identity) approves(username string) bool {
	if username == "" {
		return false
	}

	_, listed := i.approvers[strings.ToLower(username)]

	return listed
}

// noteLoopTrip logs the first refusal of a cooldown and stays quiet for the
// rest of it, so a conversation that keeps posting writes one line rather than
// one per post (FR15).
func (i *identity) noteLoopTrip(ctx context.Context, log *slog.Logger, key string) {
	if i.tripped[key] {
		return
	}

	i.tripped[key] = true

	log.WarnContext(ctx, "mattermost bot loop limiter tripped",
		"identity", i.name, "chat_key", key)
}

// clearLoopTrip forgets a conversation's cooldown once it is answering again,
// so a later trip logs again.
func (i *identity) clearLoopTrip(key string) {
	delete(i.tripped, key)
}
