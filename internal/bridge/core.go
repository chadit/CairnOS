package bridge

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/store"
)

// Update outcome labels for the updates_total counter. They are exported because
// an adapter refuses some traffic before the core ever sees it and records that
// refusal itself, so the vocabulary has to be one list rather than one per
// transport.
const (
	// OutcomeHandled counts one inbound a conversation worker accepted.
	OutcomeHandled = "handled"

	// OutcomeUnauthorized counts one inbound the transport dropped because the
	// sender is not allowlisted.
	OutcomeUnauthorized = "unauthorized"

	// OutcomeError counts one inbound the transport could not place or the core
	// could not queue.
	OutcomeError = "error"
)

const (
	// inboxSize bounds a per-conversation worker's backlog. A full inbox drops the
	// inbound rather than blocking the transport's reader and stalling every other
	// conversation (FR20).
	inboxSize = 16

	// turnEventBuffer bounds the per-turn event channel the agent streams into.
	turnEventBuffer = 32

	// Bridge commands a message can carry instead of chat text.
	cmdNew     = "/new"
	cmdSession = "/session"

	// keySep joins the parts of a conversation key. It is not a transport value,
	// only the core's own worker-map key, so any separator would do.
	keySep = "|"
)

// Address names one conversation on one transport for one identity. ChatKey is
// the bridge_sessions key the session binding is stored under; Channel and
// Thread are the transport's reply coordinates and stay opaque to the core.
type Address struct {
	// Transport is the store binding's transport column value: telegram,
	// mattermost, or a2a.
	Transport string

	// Identity is the config identity name a multi-identity transport answers as.
	// Telegram and A2A have a single identity and leave it empty.
	Identity string

	// ChatKey identifies the conversation within the transport and identity.
	ChatKey string

	// Channel and Thread are the transport's own reply coordinates, passed back
	// verbatim on every Outbound call.
	Channel string
	Thread  string
}

// Sender identifies who sent one inbound, as the transport resolved them. ID is
// the opener identity a proposal is matched against, so it must be stable for a
// given person on that transport.
type Sender struct {
	ID    string
	Name  string
	IsBot bool
}

// Grant is what the transport resolved for this sender: the Telegram allowlist
// entry, the Mattermost identity's workspace plus approver check, or the A2A
// token's scopes. Authorization is operator-set config rather than a per-request
// token scope, so the transport decides and the core enforces (doc.go).
type Grant struct {
	// Workspace is stamped onto every turn's context, exactly as the gRPC
	// interceptor stamps a verified token's workspace.
	Workspace string

	// Personality is the chat personality a session created for this sender gets.
	Personality string

	// AllowConfirm lets this sender approve a D9 proposal. Denying never needs it.
	AllowConfirm bool
}

// Kind separates an ordinary inbound from an explicit decision a transport
// resolved on its own, such as a Telegram inline-keyboard tap.
type Kind int

const (
	// KindMessage is a turn, a bridge command, or a typed approve/deny.
	KindMessage Kind = iota

	// KindDecision is an explicit approve or deny the transport already parsed,
	// carrying the session it targets in Inbound.Session.
	KindDecision
)

// Inbound is one message a transport routed to the core. Out rides the inbound
// rather than living in a per-transport map because A2A captures replies per
// request while Telegram and Mattermost pass one long-lived adapter.
//
// Field order is size-descending for govet's fieldalignment check rather than
// reading order; Addr, Sender, and Grant are the three that carry weight.
type Inbound struct {
	Out     Outbound
	Addr    Address
	Text    string
	Session string
	Sender  Sender
	Grant   Grant
	Kind    Kind
	Approve bool
}

// Outbound is the transport's send surface for one address. The core never
// formats wire payloads: it hands over text or a proposal and gets back a
// reference it can later expire. Addresses are passed by pointer because
// Address is five strings and every send would otherwise copy all of them; an
// implementation MUST treat it as read-only.
type Outbound interface {
	// Send posts one message and returns the transport's reference for it, which
	// may be empty when the transport has nothing to edit later.
	Send(ctx context.Context, addr *Address, text string) (string, error)

	// SendProposal renders a D9 proposal however the transport asks for a
	// decision, returning the reference an Expirer is later called with.
	SendProposal(ctx context.Context, addr *Address, sessionID string, proposal *chat.Proposal) (string, error)

	// MaxChars is the transport's per-message limit, counted in runes.
	MaxChars() int
}

// Expirer is the optional half of Outbound a transport implements when a lapsed
// proposal leaves a posted message to settle. A request-scoped transport, as
// A2A is, answered long before the TTL and has nothing left to mark.
type Expirer interface {
	// Expired marks a lapsed proposal so the operator sees it settled rather
	// than still awaiting an answer.
	Expired(ctx context.Context, addr *Address, ref string) error
}

// TurnCompleter is the optional half of Outbound a request-scoped transport
// implements. Handle is asynchronous, so a transport answering one request per
// inbound, as A2A does, has no other way to know its reply is complete. A
// poll-loop transport posts as replies arrive and implements nothing.
type TurnCompleter interface {
	// Done fires once per inbound, after the turn or decision finishes sending
	// and also for an inbound the core drops, so a caller blocked on the signal
	// never waits out a turn that will not run.
	Done()
}

// completeTurn signals one inbound's send surface that its turn is over, when
// that surface asks to be told. It is the only place the optional hook is
// consulted, so every exit path out of one inbound routes through here.
func completeTurn(out Outbound) {
	if completer, ok := out.(TurnCompleter); ok {
		completer.Done()
	}
}

// ChatService is the slice of the chat agent the bridge drives; *chat.Agent
// satisfies it. Defined here so the bridge tests run against a fake agent and
// the dependency points at a narrow surface, mirroring internal/api's ChatAgent.
type ChatService interface {
	CreateSession(ctx context.Context, personality string) (*store.ChatSession, error)
	GetSession(ctx context.Context, id string) (*store.ChatSession, error)
	SendMessage(ctx context.Context, sessionID, userMessage string, out chan<- chat.Event) error
	ConfirmAction(ctx context.Context, sessionID string, approve bool, out chan<- chat.Event) error
}

// Metrics is the telemetry surface the bridge records through, kept an interface
// so the package stays telemetry-free; the cli wiring adapts telemetry.Recorder
// the way queueMetricsAdapter does. Every method carries the transport and the
// identity so one dashboard splits Telegram from each Mattermost bot (FR42).
type Metrics interface {
	// Update counts one inbound by outcome: handled, unauthorized, or error. The
	// transport records what it refuses before the core sees it; the core records
	// what its workers accept and what a full inbox drops.
	Update(transport, identity, outcome string)
	// Turn counts one delivered chat turn, ok false on a turn failure.
	Turn(transport, identity string, ok bool)
	// Confirm counts one resolved D9 confirmation by outcome. The confirm slice
	// (confirm.go) drives it.
	Confirm(transport, identity, outcome string)
	// SendFailure counts one outbound send that failed after the turn completed.
	SendFailure(transport, identity string)
}

// CoreConfig wires one Core. Every field is transport-neutral: what a given
// transport needs to reach its wire lives in that transport's own adapter.
type CoreConfig struct {
	// Chat is the agent the core drives; the shipped *chat.Agent satisfies it.
	Chat ChatService

	// Store holds the conversation-to-session bindings that survive a restart.
	Store store.BridgeSessionStore

	// Metrics records the four bridge counters; nil is not allowed.
	Metrics Metrics

	// Logger receives the core's structured logs; nil falls back to the slog
	// default.
	Logger *slog.Logger

	// ConfirmTTL is how long an unanswered D9 proposal stays pending; the confirm
	// slice (confirm.go) arms the timer.
	ConfirmTTL time.Duration

	// Verbose relays tool-call status lines when true.
	Verbose bool
}

// Core is the transport-neutral half of the bridge: session binding, one worker
// per conversation, the turn fold, and the D9 confirmation gate. Every transport
// hands it Inbound values and gets its replies back through that inbound's
// Outbound, so the core never touches a wire format.
type Core struct {
	chat    ChatService
	store   store.BridgeSessionStore
	metrics Metrics
	log     *slog.Logger

	// pending is the confirm routing map: session id -> the address, send surface,
	// message reference, and opener of a live proposal, plus its expiry timer. The
	// D9 gate state itself stays inside chat.Agent; this holds only what the core
	// needs to route a decision and mark the message on expiry (confirm.go).
	pending map[string]*route

	// workers maps a conversation key to its serial processor.
	workers map[string]*worker

	// waiters tracks the live conversation workers so Drain can wait for them.
	waiters sync.WaitGroup

	confirmTTL time.Duration

	// confirmMu guards pending, which the conversation workers and the TTL timer
	// callbacks touch concurrently.
	confirmMu sync.Mutex

	// workerMu guards workers and drained, which every transport's reader touches
	// concurrently once more than one transport runs.
	workerMu sync.Mutex

	verbose bool

	// drained records that Drain already closed the worker inboxes, so a second
	// Drain is a no-op and a late Handle drops instead of resurrecting a worker
	// nothing will wait for.
	drained bool
}

// NewCore builds a Core from cfg.
func NewCore(cfg *CoreConfig) *Core {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	return &Core{
		chat:       cfg.Chat,
		store:      cfg.Store,
		metrics:    cfg.Metrics,
		log:        log,
		pending:    map[string]*route{},
		workers:    map[string]*worker{},
		confirmTTL: cfg.ConfirmTTL,
		verbose:    cfg.Verbose,
	}
}

// worker is one conversation's serial processor: its inbounds run FIFO on their
// own goroutine, so a slow turn occupies only that conversation while every
// transport reader and every other conversation keeps moving.
type worker struct {
	// The queue carries a closure rather than the Inbound because each transport
	// hands the core a different context lifetime: a poll loop passes one that
	// lives as long as serve, A2A passes the request's. Binding the worker's
	// creating context onto every later inbound would cancel one transport's turn
	// when another's request ended.
	inbox chan job
}

// job is one queued unit of a conversation's work. The address rides along so the
// panic recovery in runJob can name the conversation whose turn died.
type job struct {
	run  func()
	addr Address
}

// queueResult reports what enqueue did with one inbound.
type queueResult int

const (
	// queueAccepted means the conversation worker took the inbound.
	queueAccepted queueResult = iota

	// queueDrained means Drain already closed the workers, so nothing will run it.
	queueDrained

	// queueFull means the conversation's backlog is at inboxSize (FR20).
	queueFull
)

// Handle routes one inbound to its conversation worker, starting that worker on
// first contact. A full inbox drops the newest inbound and counts it rather than
// blocking the caller's reader (FR20). It is the only entry point for every
// transport and is safe to call concurrently, Drain included: enqueue queues
// under the same lock Drain closes the inboxes with.
func (c *Core) Handle(ctx context.Context, inbound *Inbound) {
	switch c.enqueue(ctx, inbound) {
	case queueAccepted:
		return
	case queueDrained:
		c.log.DebugContext(ctx, "bridge drained, dropping inbound",
			"transport", inbound.Addr.Transport, "chat_key", inbound.Addr.ChatKey)
	case queueFull:
		c.metrics.Update(inbound.Addr.Transport, inbound.Addr.Identity, OutcomeError)
		c.log.WarnContext(ctx, "conversation inbox full, dropping inbound",
			"transport", inbound.Addr.Transport, "chat_key", inbound.Addr.ChatKey)
	}

	completeTurn(inbound.Out)
}

// Drain closes every conversation worker, waits for the queued work to finish,
// and stops every armed proposal timer, so no goroutine and no timer outlives the
// transport that owned them. Each transport's Run calls it from its deferred
// cleanup; a second call is a no-op.
func (c *Core) Drain() {
	c.closeWorkers()
	c.waiters.Wait()
	c.stopAllProposals()
}

// enqueue offers an inbound to its conversation worker, starting it on first
// contact. The lock spans the drained check, the map read, and the send because
// closeWorkers closes the inboxes under the same lock: a send to a closed channel
// panics whether or not the select has a default arm. The send never blocks, so
// the mutex costs nothing.
func (c *Core) enqueue(ctx context.Context, inbound *Inbound) queueResult {
	c.workerMu.Lock()
	defer c.workerMu.Unlock()

	if c.drained {
		return queueDrained
	}

	target := c.workerForLocked(conversationKey(&inbound.Addr))

	select {
	case target.inbox <- job{addr: inbound.Addr, run: func() { c.process(ctx, inbound) }}:
		return queueAccepted
	default:
		return queueFull
	}
}

// closeWorkers closes each live worker inbox once and marks the core drained, so
// the workers finish their backlog and exit and no later Handle starts a worker
// that Drain is no longer waiting for.
func (c *Core) closeWorkers() {
	c.workerMu.Lock()
	defer c.workerMu.Unlock()

	if c.drained {
		return
	}

	c.drained = true

	for _, active := range c.workers {
		close(active.inbox)
	}

	c.workers = map[string]*worker{}
}

// workerForLocked returns the conversation's worker, starting one on first
// contact. The caller holds workerMu and has already checked drained.
func (c *Core) workerForLocked(key string) *worker {
	if active, ok := c.workers[key]; ok {
		return active
	}

	active := &worker{inbox: make(chan job, inboxSize)}
	c.workers[key] = active

	c.waiters.Add(1)

	go c.runWorker(active)

	return active
}

// runWorker processes one conversation's queued work in arrival order until its
// inbox closes.
func (c *Core) runWorker(active *worker) {
	defer c.waiters.Done()

	for queued := range active.inbox {
		c.runJob(&queued)
	}
}

// runJob runs one queued turn under a recover, so a panic in a driver call, a
// formatter, or a transport send costs that one conversation its turn instead of
// taking serve, the API server, and every other identity down with it (NFR3).
// process defers the completion signal, so a request-scoped caller is released
// during the unwind rather than left waiting.
func (c *Core) runJob(queued *job) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}

		c.log.Error("bridge turn panicked",
			"panic", recovered,
			"transport", queued.addr.Transport,
			"identity", queued.addr.Identity,
			"chat_key", queued.addr.ChatKey,
			"stack", string(debug.Stack()))
	}()

	queued.run()
}

// conversationKey names the worker that owns one conversation. Transport and
// identity join the chat key because two transports, or two identities on one
// server, can hand the core the same chat key for unrelated conversations.
func conversationKey(addr *Address) string {
	return addr.Transport + keySep + addr.Identity + keySep + addr.ChatKey
}
