package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
)

// chatHelpText is the /help table. It states personality immutability
// outright because /personality <name> opening a fresh session would
// otherwise read as a bug.
const chatHelpText = `commands:
  /help                show this help
  /new [personality]   start a new session, optionally with a personality
  /session             show the current session id and personality
  /session list        show recent sessions, newest first
  /session <id>        switch to an existing session
  /personality         show the current session's personality
  /personality <name>  start a new session with that personality
                       (personality is immutable per session)
  /search <text>       full-text search across the workspace's transcripts
  /quit, /exit         leave the chat
`

// chatClient is the slice of ChatServiceClient the REPL drives; the real
// grpc client satisfies it, tests fake it without a network.
type chatClient interface {
	CreateSession(ctx context.Context, req *cairnosv1.CreateSessionRequest, opts ...grpc.CallOption) (*cairnosv1.CreateSessionResponse, error)
	GetSession(ctx context.Context, req *cairnosv1.GetSessionRequest, opts ...grpc.CallOption) (*cairnosv1.GetSessionResponse, error)
	ListSessions(ctx context.Context, req *cairnosv1.ListSessionsRequest, opts ...grpc.CallOption) (*cairnosv1.ListSessionsResponse, error)
	SendMessage(ctx context.Context, req *cairnosv1.SendMessageRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[cairnosv1.ChatEvent], error)
	ConfirmAction(ctx context.Context, req *cairnosv1.ConfirmActionRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[cairnosv1.ChatEvent], error)
	SearchMessages(ctx context.Context, req *cairnosv1.SearchMessagesRequest, opts ...grpc.CallOption) (*cairnosv1.SearchMessagesResponse, error)
}

// chatREPL holds one interactive conversation's state.
type chatREPL struct {
	app         *App // output and errors go through the App printers
	client      chatClient
	sessionID   string
	personality string

	// scanner is the one scanner over the App's stdin; message reads and y/n
	// approvals share it, because a second scanner over the same reader
	// cannot see whatever the first one buffered past the current line.
	scanner *bufio.Scanner

	mu         sync.Mutex
	cancelTurn context.CancelFunc // non-nil only while a turn is in flight
}

// newChatREPL wires one conversation over a single shared scanner, so the
// message loop and the approval prompt never race two buffered readers over
// the same stdin.
func newChatREPL(app *App, client chatClient, sessionID, personality string) *chatREPL {
	return &chatREPL{
		app:         app,
		client:      client,
		sessionID:   sessionID,
		personality: personality,
		scanner:     bufio.NewScanner(app.stdin),
	}
}

// run reads lines until /quit, /exit, stdin EOF, or a scanner error. Parent
// cancellation deliberately does not end the loop (cmdChat detaches it with
// context.WithoutCancel), so those four exits are the whole shutdown story.
func (r *chatREPL) run(ctx context.Context, interrupts <-chan os.Signal) int {
	done := make(chan struct{})
	watcherStopped := make(chan struct{})

	go func() {
		defer close(watcherStopped)

		r.watchInterrupts(interrupts, done)
	}()

	// Join the watcher before returning so a late interrupt never prints
	// through the App after the REPL is gone.
	defer func() {
		close(done)
		<-watcherStopped
	}()

	return r.loop(ctx)
}

// loop is run's read-dispatch-send cycle, split out so run stays about the
// watcher's lifetime.
func (r *chatREPL) loop(ctx context.Context) int {
	for {
		r.app.printOutf("> ")

		if !r.scanner.Scan() {
			return r.scannerExit()
		}

		line := strings.TrimSpace(r.scanner.Text())
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "/") {
			if quit := r.dispatch(ctx, line); quit {
				return ExitOK
			}

			continue
		}

		message, sendable := r.readMultiline(line)
		if !sendable {
			// EOF mid-block: send nothing and exit cleanly.
			return r.scannerExit()
		}

		if err := r.runTurn(ctx, message); err != nil {
			// A turn error is not an exit path: the session survives
			// server-side, so print the error and offer the next prompt
			// instead of letting a transient stream failure kill the REPL.
			r.app.printErrf("chat: %v\n", err)
		}
	}
}

// scannerExit reports the exit code after the input stream ends: a scan error
// fails, a clean EOF (the operator closed stdin) exits successfully.
func (r *chatREPL) scannerExit() int {
	if err := r.scanner.Err(); err != nil {
		r.app.printErrf("chat: read input: %v\n", err)

		return ExitFailure
	}

	r.app.printOutf("\n")

	return ExitOK
}

// dispatch runs one slash command; these never reach the model. It reports
// whether the REPL should exit (only /quit and /exit ask for that).
func (r *chatREPL) dispatch(ctx context.Context, line string) bool {
	command, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)

	switch command {
	case "/help":
		r.app.printOutf("%s", chatHelpText)
	case "/new":
		r.switchToNewSession(ctx, arg)
	case "/session":
		r.commandSession(ctx, arg)
	case "/personality":
		r.commandPersonality(ctx, arg)
	case "/search":
		r.commandSearch(ctx, arg)
	case "/quit", "/exit":
		return true
	default:
		r.app.printOutf("unknown command, try /help\n")
	}

	return false
}

// commandSession handles /session: bare prints the current session, "list"
// lists recent sessions, anything else switches to that session id.
func (r *chatREPL) commandSession(ctx context.Context, arg string) {
	switch arg {
	case "":
		r.app.printOutf("session %s (%s)\n", r.sessionID, r.personality)
	case "list":
		r.listSessions(ctx)
	default:
		r.switchToSession(ctx, arg)
	}
}

// commandPersonality prints the current personality, or starts a new session
// with the named one: personality is immutable per session, so a change
// always means a fresh session.
func (r *chatREPL) commandPersonality(ctx context.Context, name string) {
	if name == "" {
		r.app.printOutf("personality %s\n", r.personality)

		return
	}

	r.switchToNewSession(ctx, name)
}

// switchToNewSession opens a fresh session, optionally with a personality,
// and moves the REPL onto it.
func (r *chatREPL) switchToNewSession(ctx context.Context, personality string) {
	resp, err := r.client.CreateSession(ctx, &cairnosv1.CreateSessionRequest{Personality: personality})
	if err != nil {
		r.app.printErrf("chat: create session: %v\n", err)

		return
	}

	r.adoptSession(resp.GetSession())
}

// switchToSession moves the REPL onto an existing session; on failure (for
// example NotFound) it prints the error and stays on the current session.
func (r *chatREPL) switchToSession(ctx context.Context, sessionID string) {
	resp, err := r.client.GetSession(ctx, &cairnosv1.GetSessionRequest{SessionId: sessionID})
	if err != nil {
		r.app.printErrf("chat: get session %s: %v\n", sessionID, err)

		return
	}

	r.adoptSession(resp.GetSession())
}

// adoptSession points the REPL at the given session and echoes the switch.
func (r *chatREPL) adoptSession(session *cairnosv1.ChatSession) {
	r.sessionID = session.GetId()
	r.personality = session.GetPersonality()
	r.app.printOutf("session %s (%s)\n", r.sessionID, r.personality)
}

// listSessions prints the workspace's sessions; the server already orders
// them most-recently-updated first, so print order is arrival order.
func (r *chatREPL) listSessions(ctx context.Context) {
	resp, err := r.client.ListSessions(ctx, &cairnosv1.ListSessionsRequest{})
	if err != nil {
		r.app.printErrf("chat: list sessions: %v\n", err)

		return
	}

	for _, session := range resp.GetSessions() {
		r.app.printOutf("session %s (%s) updated %s\n",
			session.GetId(), session.GetPersonality(),
			session.GetUpdatedAt().AsTime().Format(time.RFC3339))
	}
}

// commandSearch handles /search: full-text search across every session in
// the workspace, no session filter on purpose, because "where did we talk
// about X" rarely knows which session it happened in.
func (r *chatREPL) commandSearch(ctx context.Context, text string) {
	if text == "" {
		// Say what /search wants locally instead of round-tripping an empty
		// query the server would reject with InvalidArgument anyway.
		r.app.printOutf("usage: /search <text>\n")

		return
	}

	resp, err := r.client.SearchMessages(ctx, &cairnosv1.SearchMessagesRequest{Text: text, Limit: defaultChatSearchLimit})
	if err != nil {
		r.app.printErrf("chat: search: %v\n", err)

		return
	}

	r.app.printSearchHits(resp.GetMessages())
}

// printSearchHits prints search hits one per line in the shared format both
// the REPL's /search and the one-shot --search flag use:
//
//	session <id> #<seq> [role] <text>
func (a *App) printSearchHits(messages []*cairnosv1.ChatMessage) {
	for _, message := range messages {
		a.printOutf("session %s #%d [%s] %s\n",
			message.GetSessionId(), message.GetSeq(), message.GetRole(), message.GetContent())
	}
}

// readMultiline assembles one message from continuation lines: a single
// trailing backslash strips and continues on a "... " prompt, and the
// fragments join with newlines so the block sends as one SendMessage. The
// bool is false when stdin ended mid-block, meaning nothing should be sent.
func (r *chatREPL) readMultiline(first string) (string, bool) {
	fragment, more := splitContinuation(first)
	if !more {
		return fragment, true
	}

	parts := []string{fragment}

	for {
		r.app.printOutf("... ")

		if !r.scanner.Scan() {
			return "", false
		}

		fragment, more = splitContinuation(r.scanner.Text())
		parts = append(parts, fragment)

		if !more {
			return strings.Join(parts, "\n"), true
		}
	}
}

// splitContinuation reports whether a line asks to continue: exactly one
// trailing backslash strips and continues, while a doubled backslash stays
// literal so users can still end a message with one.
func splitContinuation(line string) (string, bool) {
	if strings.HasSuffix(line, `\`) && !strings.HasSuffix(line, `\\`) {
		return strings.TrimSuffix(line, `\`), true
	}

	return line, false
}

// runTurn sends one message under a per-turn cancel so an interrupt cuts this
// turn without ending the session or the process.
func (r *chatREPL) runTurn(ctx context.Context, message string) error {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	r.armTurn(cancel)

	err := r.sendMessage(turnCtx, message)

	r.disarmTurn()

	if err != nil && turnCtx.Err() != nil {
		// The user message persisted before the turn was cut, so the next
		// turn's history includes the orphan; say so instead of hiding it.
		r.app.printOutf("turn interrupted; the last message is saved without a reply\n")

		return nil
	}

	return err
}

// armTurn publishes the in-flight turn's cancel so the interrupt watcher can
// cut it.
func (r *chatREPL) armTurn(cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.cancelTurn = cancel
}

// disarmTurn clears the published cancel once the turn settles, so a later
// interrupt prints the idle hint instead of canceling a dead context.
func (r *chatREPL) disarmTurn() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.cancelTurn = nil
}

// watchInterrupts owns the interrupt channel for the REPL's lifetime and
// stops when done closes, so a real signal.Notify channel never strands the
// goroutine after the REPL exits.
func (r *chatREPL) watchInterrupts(interrupts <-chan os.Signal, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case <-interrupts:
			r.interruptNow()
		}
	}
}

// interruptNow cancels the in-flight turn when one exists; at the prompt it
// prints how to leave, because SIGINT no longer kills the process.
func (r *chatREPL) interruptNow() {
	r.mu.Lock()
	cancel := r.cancelTurn
	r.mu.Unlock()

	if cancel != nil {
		cancel()

		return
	}

	r.app.printOutf("interrupt: type /quit to exit\n")
}

// sendMessage sends one message, streams its events, and on a
// confirmation-required event prompts the operator and confirms or denies.
func (r *chatREPL) sendMessage(ctx context.Context, message string) error {
	stream, err := r.client.SendMessage(ctx, &cairnosv1.SendMessageRequest{SessionId: r.sessionID, Message: message})
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}

	pending, gated, err := r.streamEvents(stream)
	if err != nil {
		return err
	}

	if !gated {
		return nil
	}

	return r.resolveProposal(ctx, pending)
}

// chatEventStream is the receive side of SendMessage and ConfirmAction, so one
// streamEvents body serves both.
type chatEventStream interface {
	Recv() (*cairnosv1.ChatEvent, error)
}

// streamEvents prints assistant text and tool-call notices as they arrive. The
// bool reports whether the turn paused for confirmation, in which case the
// proposal is returned; a false bool means the turn answered or ran to
// completion.
func (r *chatREPL) streamEvents(stream chatEventStream) (*cairnosv1.WorkflowProposal, bool, error) {
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil, false, nil
		}

		if err != nil {
			return nil, false, fmt.Errorf("receive chat event: %w", err)
		}

		if proposal := r.printChatEvent(event); proposal != nil {
			return proposal, true, nil
		}
	}
}

// printChatEvent prints one event for the operator and returns a proposal when
// the event is a confirmation gate, so the caller can prompt.
func (r *chatREPL) printChatEvent(event *cairnosv1.ChatEvent) *cairnosv1.WorkflowProposal {
	switch event.GetType() {
	case cairnosv1.ChatEventType_CHAT_EVENT_TYPE_ASSISTANT_CHUNK:
		r.app.printOutf("%s\n", event.GetText())
	case cairnosv1.ChatEventType_CHAT_EVENT_TYPE_TOOL_CALL:
		r.app.printOutf("[tool %s]\n", event.GetTool())
	case cairnosv1.ChatEventType_CHAT_EVENT_TYPE_CONFIRMATION_REQUIRED:
		return event.GetProposal()
	case cairnosv1.ChatEventType_CHAT_EVENT_TYPE_RUN_STARTED:
		r.app.printOutf("[run %s started]\n", event.GetRunId())
	case cairnosv1.ChatEventType_CHAT_EVENT_TYPE_RUN_FINISHED:
		r.app.printOutf("[run %s %s]\n", event.GetRunId(), event.GetRunStatus())
	case cairnosv1.ChatEventType_CHAT_EVENT_TYPE_TURN_DONE,
		cairnosv1.ChatEventType_CHAT_EVENT_TYPE_UNSPECIFIED:
		// Turn-done carries no new text to print here; the assistant chunk
		// already showed the reply.
	}

	return nil
}

// resolveProposal prompts the operator y/n for a pending workflow trigger and
// confirms or denies it, streaming the confirm turn's events.
func (r *chatREPL) resolveProposal(ctx context.Context, proposal *cairnosv1.WorkflowProposal) error {
	r.app.printOutf("Run workflow %q (estimated budget $%.2f)? [y/N] ",
		proposal.GetWorkflowName(), proposal.GetEstimatedBudgetUsd())

	approve := r.readApproval()

	stream, err := r.client.ConfirmAction(ctx, &cairnosv1.ConfirmActionRequest{SessionId: r.sessionID, Approve: approve})
	if err != nil {
		return fmt.Errorf("confirm action: %w", err)
	}

	if _, _, err := r.streamEvents(stream); err != nil {
		return err
	}

	return nil
}

// readApproval reads one y/n line through the shared scanner, defaulting to
// deny on anything but an explicit yes, so a stray newline never fires a
// workflow. Sharing the scanner matters: a second scanner over the same stdin
// used to swallow answers the first had already buffered.
func (r *chatREPL) readApproval() bool {
	if !r.scanner.Scan() {
		return false
	}

	answer := strings.ToLower(strings.TrimSpace(r.scanner.Text()))

	return answer == "y" || answer == "yes"
}
