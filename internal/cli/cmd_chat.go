package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
)

const chatUsage = "usage: cairnos chat [--addr host:port] [--token T] [--session id] [--personality name] [--search q] [--limit n]\n"

// defaultChatAddr is the gRPC address the chat client dials when --addr is
// unset: the serve default on loopback.
const defaultChatAddr = "127.0.0.1:7777"

// defaultChatSearchLimit caps search hits when the caller does not say
// otherwise; /search and the --limit default share it so both surfaces
// agree on how much transcript floods the terminal.
const defaultChatSearchLimit = 20

// flagSearch is looked up after parsing as well as registered, because only
// flag.Visit can tell an explicit --search "" from an absent flag.
const flagSearch = "search"

// cmdChat is the interactive chat REPL (.agents/spec/13-roadmap.md, Phase 9):
// it dials a serve API, opens or continues a session, and hands the
// conversation to chatREPL, which owns the line loop, slash commands, and
// streaming. It is a thin client over ChatService, so the agent, gate, and
// budget all live server-side. --search skips the REPL: one query, hits
// printed, exit.
func (a *App) cmdChat(ctx context.Context, args []string) int {
	opts, code, proceed := a.parseChatFlags(args)
	if !proceed {
		return code
	}

	conn, err := a.dialServe(opts.addr)
	if err != nil {
		a.printErrf("chat: %v\n", err)

		return ExitFailure
	}
	defer a.closeConn(conn)

	client := cairnosv1.NewChatServiceClient(conn)

	if opts.searchSet {
		// The one-shot stays on main's signal context: Ctrl-C should kill a
		// non-interactive query the normal way, unlike the REPL below.
		return a.runChatSearch(bearerContext(ctx, opts.token), client, &opts)
	}

	// WithoutCancel detaches the conversation from main's NotifyContext, since
	// SIGINT must interrupt one turn, not the whole session. main cancels on
	// SIGTERM and SIGHUP too, for serve's drain, so this context watches those
	// two again and kill or a closed terminal still ends the session.
	sessionCtx, stopSession := signal.NotifyContext(context.WithoutCancel(ctx), syscall.SIGTERM, syscall.SIGHUP)
	defer stopSession()

	callCtx := bearerContext(sessionCtx, opts.token)

	sessionID, personality, err := resolveSession(callCtx, client, &opts)
	if err != nil {
		a.printErrf("chat: %v\n", err)

		return ExitFailure
	}

	a.printOutf("session %s (%s); type a message, /help for commands, /quit to exit\n", sessionID, personality)

	interrupts := a.interrupts
	if interrupts == nil {
		// No injected seam, so watch the real signal for the REPL's lifetime.
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt)

		defer signal.Stop(signals)

		interrupts = signals
	}

	repl := newChatREPL(a, client, sessionID, personality)

	return a.untilStopped(sessionCtx, func() int { return repl.run(callCtx, interrupts) })
}

// untilStopped returns the REPL's exit code, or ExitOK once a stop signal ends
// ctx. The REPL blocks on a stdin read no context can interrupt, so a stop
// leaves its goroutine parked there, and main exits right after this returns.
func (a *App) untilStopped(ctx context.Context, repl func() int) int {
	exited := make(chan int, 1)

	go func() {
		exited <- repl()
	}()

	select {
	case code := <-exited:
		return code
	case <-ctx.Done():
		// End the "> " prompt line the way a stdin EOF does.
		a.printOutf("\n")

		return ExitOK
	}
}

// chatOptions holds the parsed chat flags.
type chatOptions struct {
	addr        string
	token       string
	session     string
	personality string
	search      string
	limit       int

	// searchSet distinguishes --search "" (a usage error) from --search
	// absent (enter the REPL); the flag package reports both as "".
	searchSet bool
}

// parseChatFlags parses the chat command flags.
func (a *App) parseChatFlags(args []string) (chatOptions, int, bool) {
	flags := flag.NewFlagSet("chat", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	addr := flags.String("addr", defaultChatAddr, "serve gRPC address")
	token := flags.String("token", "", "bearer token (when the server runs with auth)")
	session := flags.String("session", "", "continue an existing session id; with --search, narrow hits to it")
	personality := flags.String("personality", "", "personality for a new session")
	search := flags.String(flagSearch, "", "print matching transcript messages and exit without the REPL")
	limit := flags.Int("limit", defaultChatSearchLimit, "result cap for --search")

	if code, proceed := parseFlags(flags, args); !proceed {
		return chatOptions{}, code, false
	}

	if flags.NArg() > 0 {
		a.printErrf("%s", chatUsage)

		return chatOptions{}, ExitUsage, false
	}

	var searchSet bool

	flags.Visit(func(visited *flag.Flag) {
		if visited.Name == flagSearch {
			searchSet = true
		}
	})

	if searchSet && strings.TrimSpace(*search) == "" {
		// Fail before dialing: an empty query can only ever be a mistake.
		a.printErrf("%s", chatUsage)

		return chatOptions{}, ExitUsage, false
	}

	return chatOptions{
		addr:        *addr,
		token:       *token,
		session:     *session,
		personality: *personality,
		search:      *search,
		limit:       *limit,
		searchSet:   searchSet,
	}, ExitOK, true
}

// runChatSearch answers --search in one shot: run the query with the
// --session scope and --limit cap, print hits in the REPL's shared format,
// and exit without touching sessions or entering the line loop.
func (a *App) runChatSearch(ctx context.Context, client chatSessions, opts *chatOptions) int {
	resp, err := client.SearchMessages(ctx, &cairnosv1.SearchMessagesRequest{
		Text:      opts.search,
		SessionId: opts.session,
		// #nosec G115 -- an operator-typed result cap; a wrapped value only
		// changes the cap, and the server treats non-positive as no cap.
		Limit: int32(opts.limit),
	})
	if err != nil {
		a.printErrf("chat: search: %v\n", err)

		return ExitFailure
	}

	a.printSearchHits(resp.GetMessages())

	return ExitOK
}

// resolveSession continues the session named on the flags, or creates a new
// one when none was given. It returns the session id and personality to
// converse in; a continued session reports its stored personality, not the
// flag value.
func resolveSession(ctx context.Context, client chatSessions, opts *chatOptions) (string, string, error) {
	if opts.session != "" {
		resp, err := client.GetSession(ctx, &cairnosv1.GetSessionRequest{SessionId: opts.session})
		if err != nil {
			return "", "", fmt.Errorf("get session %s: %w", opts.session, err)
		}

		return resp.GetSession().GetId(), resp.GetSession().GetPersonality(), nil
	}

	resp, err := client.CreateSession(ctx, &cairnosv1.CreateSessionRequest{Personality: opts.personality})
	if err != nil {
		return "", "", fmt.Errorf("create session: %w", err)
	}

	return resp.GetSession().GetId(), resp.GetSession().GetPersonality(), nil
}

// bearerContext attaches the bearer token to the outgoing call metadata when one
// was given, so the client works against a serve running with auth.
func bearerContext(ctx context.Context, token string) context.Context {
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
