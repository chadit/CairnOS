package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
)

const chatUsage = "usage: cairnos chat [--addr host:port] [--token T] [--session id] [--personality name]\n"

// defaultChatAddr is the gRPC address the chat client dials when --addr is
// unset: the serve default on loopback.
const defaultChatAddr = "127.0.0.1:7777"

// cmdChat is the interactive chat REPL (.agents/spec/13-roadmap.md, Phase 9):
// it dials a running serve API, opens or continues a session, then reads a line
// at a time, streams the assistant's reply, and prompts y/n when the agent
// proposes a workflow, confirming through ConfirmAction. It is a thin client
// over ChatService, so the agent, gate, and budget all live server-side.
func (a *App) cmdChat(ctx context.Context, args []string) int {
	opts, code, proceed := a.parseChatFlags(args)
	if !proceed {
		return code
	}

	conn, err := grpc.NewClient(opts.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		a.printErrf("chat: dial %s: %v\n", opts.addr, err)

		return ExitFailure
	}
	defer a.closeConn(conn)

	client := cairnosv1.NewChatServiceClient(conn)
	callCtx := chatContext(ctx, opts.token)

	sessionID, err := resolveSession(callCtx, client, opts)
	if err != nil {
		a.printErrf("chat: %v\n", err)

		return ExitFailure
	}

	a.printOutf("session %s (%s); type a message, or /quit to exit\n", sessionID, opts.personality)

	return a.chatLoop(callCtx, client, sessionID)
}

// chatOptions holds the parsed chat flags.
type chatOptions struct {
	addr        string
	token       string
	session     string
	personality string
}

// parseChatFlags parses the chat command flags.
func (a *App) parseChatFlags(args []string) (chatOptions, int, bool) {
	flags := flag.NewFlagSet("chat", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	addr := flags.String("addr", defaultChatAddr, "serve gRPC address")
	token := flags.String("token", "", "bearer token (when the server runs with auth)")
	session := flags.String("session", "", "continue an existing session id")
	personality := flags.String("personality", "", "personality for a new session")

	if code, proceed := parseFlags(flags, args); !proceed {
		return chatOptions{}, code, false
	}

	if flags.NArg() > 0 {
		a.printErrf("%s", chatUsage)

		return chatOptions{}, ExitUsage, false
	}

	return chatOptions{addr: *addr, token: *token, session: *session, personality: *personality}, ExitOK, true
}

// resolveSession continues the session named on the flags, or creates a new one
// when none was given. It returns the session id to converse in.
func resolveSession(ctx context.Context, client cairnosv1.ChatServiceClient, opts chatOptions) (string, error) {
	if opts.session != "" {
		resp, err := client.GetSession(ctx, &cairnosv1.GetSessionRequest{SessionId: opts.session})
		if err != nil {
			return "", fmt.Errorf("get session %s: %w", opts.session, err)
		}

		opts.personality = resp.GetSession().GetPersonality()

		return resp.GetSession().GetId(), nil
	}

	resp, err := client.CreateSession(ctx, &cairnosv1.CreateSessionRequest{Personality: opts.personality})
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}

	return resp.GetSession().GetId(), nil
}

// chatLoop reads lines until EOF or /quit, sending each as a message and
// streaming the reply.
func (a *App) chatLoop(ctx context.Context, client cairnosv1.ChatServiceClient, sessionID string) int {
	reader := bufio.NewScanner(a.stdin)

	for {
		a.printOutf("> ")

		if !reader.Scan() {
			return a.scannerExit(reader)
		}

		line := strings.TrimSpace(reader.Text())
		if line == "" {
			continue
		}

		if line == "/quit" || line == "/exit" {
			return ExitOK
		}

		if err := a.runChatTurn(ctx, client, sessionID, line); err != nil {
			a.printErrf("chat: %v\n", err)

			return ExitFailure
		}
	}
}

// scannerExit reports the exit code after the input stream ends: a scan error
// fails, a clean EOF (the operator closed stdin) exits successfully.
func (a *App) scannerExit(reader *bufio.Scanner) int {
	if err := reader.Err(); err != nil {
		a.printErrf("chat: read input: %v\n", err)

		return ExitFailure
	}

	a.printOutf("\n")

	return ExitOK
}

// runChatTurn sends one message, streams its events, and on a
// confirmation-required event prompts the operator and confirms or denies.
func (a *App) runChatTurn(ctx context.Context, client cairnosv1.ChatServiceClient, sessionID, message string) error {
	stream, err := client.SendMessage(ctx, &cairnosv1.SendMessageRequest{SessionId: sessionID, Message: message})
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}

	pending, gated, err := a.streamEvents(stream)
	if err != nil {
		return err
	}

	if !gated {
		return nil
	}

	return a.resolveProposal(ctx, client, sessionID, pending)
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
func (a *App) streamEvents(stream chatEventStream) (*cairnosv1.WorkflowProposal, bool, error) {
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil, false, nil
		}

		if err != nil {
			return nil, false, fmt.Errorf("receive chat event: %w", err)
		}

		if proposal := a.printChatEvent(event); proposal != nil {
			return proposal, true, nil
		}
	}
}

// printChatEvent prints one event for the operator and returns a proposal when
// the event is a confirmation gate, so the caller can prompt.
func (a *App) printChatEvent(event *cairnosv1.ChatEvent) *cairnosv1.WorkflowProposal {
	switch event.GetType() {
	case cairnosv1.ChatEventType_CHAT_EVENT_TYPE_ASSISTANT_CHUNK:
		a.printOutf("%s\n", event.GetText())
	case cairnosv1.ChatEventType_CHAT_EVENT_TYPE_TOOL_CALL:
		a.printOutf("[tool %s]\n", event.GetTool())
	case cairnosv1.ChatEventType_CHAT_EVENT_TYPE_CONFIRMATION_REQUIRED:
		return event.GetProposal()
	case cairnosv1.ChatEventType_CHAT_EVENT_TYPE_RUN_STARTED:
		a.printOutf("[run %s started]\n", event.GetRunId())
	case cairnosv1.ChatEventType_CHAT_EVENT_TYPE_RUN_FINISHED:
		a.printOutf("[run %s %s]\n", event.GetRunId(), event.GetRunStatus())
	case cairnosv1.ChatEventType_CHAT_EVENT_TYPE_TURN_DONE,
		cairnosv1.ChatEventType_CHAT_EVENT_TYPE_UNSPECIFIED:
		// Turn-done carries no new text to print here; the assistant chunk
		// already showed the reply.
	}

	return nil
}

// resolveProposal prompts the operator y/n for a pending workflow trigger and
// confirms or denies it, streaming the confirm turn's events.
func (a *App) resolveProposal(ctx context.Context, client cairnosv1.ChatServiceClient, sessionID string, proposal *cairnosv1.WorkflowProposal) error {
	a.printOutf("Run workflow %q (estimated budget $%.2f)? [y/N] ",
		proposal.GetWorkflowName(), proposal.GetEstimatedBudgetUsd())

	approve := a.readApproval()

	stream, err := client.ConfirmAction(ctx, &cairnosv1.ConfirmActionRequest{SessionId: sessionID, Approve: approve})
	if err != nil {
		return fmt.Errorf("confirm action: %w", err)
	}

	if _, _, err := a.streamEvents(stream); err != nil {
		return err
	}

	return nil
}

// readApproval reads one y/n line, defaulting to deny on anything but an
// explicit yes, so a stray newline never fires a workflow.
func (a *App) readApproval() bool {
	reader := bufio.NewScanner(a.stdin)
	if !reader.Scan() {
		return false
	}

	answer := strings.ToLower(strings.TrimSpace(reader.Text()))

	return answer == "y" || answer == "yes"
}

// chatContext attaches the bearer token to the outgoing call metadata when one
// was given, so the client works against a serve running with auth.
func chatContext(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}

	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

// closeConn closes the gRPC client connection, logging instead of failing: by
// close time the command's outcome is already decided.
func (a *App) closeConn(conn *grpc.ClientConn) {
	if err := conn.Close(); err != nil {
		a.logger.Warn("close chat connection", "error", err)
	}
}
