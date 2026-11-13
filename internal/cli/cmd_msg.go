package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
)

// Usage lines for the msg family. Each subverb prints its own so a missing flag
// shows the shape that flag belongs to rather than the family summary.
const (
	msgUsage     = "usage: cairnos msg <send|list> [flags]\n"
	msgSendUsage = "usage: cairnos msg send --as <identity> --to <target> " +
		"[--thread post-id] [--addr host:port] [--token T] <text|->\n"
	msgListUsage = "usage: cairnos msg list [--addr host:port] [--token T]\n"
)

// subcmdSend names the send verb; it is a literal in both the switch and the
// flag set label.
const subcmdSend = "send"

// stdinTextMarker is the positional that means "read the message body from
// stdin", the same single-dash convention cairnos secrets set uses.
const stdinTextMarker = "-"

// unixScheme prefixes a gRPC target that names a socket on this host rather
// than a network address.
const unixScheme = "unix:"

// defaultMsgAddr is the gRPC address msg dials when --addr is unset. It aliases
// the chat default rather than repeating the literal, because both clients talk
// to the same serve on loopback.
const defaultMsgAddr = defaultChatAddr

// cmdMsg is the outbound messaging client: it posts as a configured identity
// through a running serve's MessagingService, and lists the identities that
// serve holds. Everything it needs lives server-side, so the command is a
// thin gRPC client with no store and no config of its own.
func (a *App) cmdMsg(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printErrf("%s", msgUsage)

		return ExitUsage
	}

	switch args[0] {
	case subcmdSend:
		return a.cmdMsgSend(ctx, args[1:])
	case subcmdList:
		return a.cmdMsgList(ctx, args[1:])
	default:
		a.printErrf("unknown msg subcommand %q\n", args[0])
		a.printErrf("%s", msgUsage)

		return ExitUsage
	}
}

// msgSendOpts is one parsed send invocation.
type msgSendOpts struct {
	addr     string
	token    string
	identity string
	target   string
	thread   string
	text     string
}

// cmdMsgSend posts one message and prints the created post id, which is what a
// caller threads a follow-up onto.
func (a *App) cmdMsgSend(ctx context.Context, args []string) int {
	opts, code, proceed := a.parseMsgSendFlags(args)
	if !proceed {
		return code
	}

	a.warnPlaintextToken(opts.addr, opts.token)

	conn, err := a.dialServe(opts.addr)
	if err != nil {
		a.printErrf("msg send: %v\n", err)

		return ExitFailure
	}
	defer a.closeConn(conn)

	client := cairnosv1.NewMessagingServiceClient(conn)

	resp, err := client.SendMessage(bearerContext(ctx, opts.token), &cairnosv1.MessagingServiceSendMessageRequest{
		Identity: opts.identity,
		To:       opts.target,
		Text:     opts.text,
		ThreadId: opts.thread,
	})
	if err != nil {
		a.printErrf("msg send: %v\n", err)

		return ExitFailure
	}

	a.printOutf("%s\n", resp.GetPostId())

	return ExitOK
}

// parseMsgSendFlags reads the send flags and resolves the message body, so
// cmdMsgSend holds only the call and its two outcomes. Identity, target, and
// text are all checked here: dialing a serve to be told a flag was blank costs a
// round trip and reads as a delivery failure in the server's metrics.
func (a *App) parseMsgSendFlags(args []string) (msgSendOpts, int, bool) {
	flags := flag.NewFlagSet("msg send", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	addr := flags.String("addr", defaultMsgAddr, "serve gRPC address")
	token := flags.String("token", "", "bearer token (when the server runs with auth)")
	identity := flags.String("as", "", "identity to send as")
	target := flags.String("to", "", "target: #channel-name, @username, channel:<id>, or user:<id>")
	thread := flags.String("thread", "", "root post id to reply into")

	code, proceed := parseFlags(flags, args)
	if !proceed {
		return msgSendOpts{}, code, false
	}

	if *identity == "" || *target == "" {
		a.printErrf("msg send: --as and --to are both required\n")
		a.printErrf("%s", msgSendUsage)

		return msgSendOpts{}, ExitUsage, false
	}

	text, code, proceed := a.msgText(flags.Args())
	if !proceed {
		return msgSendOpts{}, code, false
	}

	return msgSendOpts{
		addr:     *addr,
		token:    *token,
		identity: *identity,
		target:   *target,
		thread:   *thread,
		text:     text,
	}, ExitOK, true
}

// msgText resolves the message body from the positional arguments: a lone "-"
// reads stdin, anything else joins the words back together, because a shell
// splits an unquoted message and the server wants one string.
func (a *App) msgText(args []string) (string, int, bool) {
	if len(args) == 0 {
		a.printErrf("msg send: no message text\n")
		a.printErrf("%s", msgSendUsage)

		return "", ExitUsage, false
	}

	text, code, proceed := a.joinOrReadText(args)
	if !proceed {
		return "", code, false
	}

	if strings.TrimSpace(text) == "" {
		a.printErrf("msg send: message text is empty\n")

		return "", ExitUsage, false
	}

	return text, ExitOK, true
}

// joinOrReadText picks the body's source. The stdin read is the path a pipeline
// uses, so a read failure is a runtime failure rather than a usage error.
func (a *App) joinOrReadText(args []string) (string, int, bool) {
	if len(args) != 1 || args[0] != stdinTextMarker {
		return strings.Join(args, " "), ExitOK, true
	}

	raw, err := io.ReadAll(a.stdin)
	if err != nil {
		a.printErrf("msg send: read text from stdin: %v\n", err)

		return "", ExitFailure, false
	}

	return trimOneTrailingNewline(string(raw)), ExitOK, true
}

// cmdMsgList prints the identities a caller may send as, which is how an
// operator finds the name --as wants.
func (a *App) cmdMsgList(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("msg list", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	addr := flags.String("addr", defaultMsgAddr, "serve gRPC address")
	token := flags.String("token", "", "bearer token (when the server runs with auth)")

	code, proceed := parseFlags(flags, args)
	if !proceed {
		return code
	}

	if flags.NArg() > 0 {
		a.printErrf("%s", msgListUsage)

		return ExitUsage
	}

	a.warnPlaintextToken(*addr, *token)

	conn, err := a.dialServe(*addr)
	if err != nil {
		a.printErrf("msg list: %v\n", err)

		return ExitFailure
	}
	defer a.closeConn(conn)

	resp, err := cairnosv1.NewMessagingServiceClient(conn).
		ListIdentities(bearerContext(ctx, *token), &cairnosv1.ListIdentitiesRequest{})
	if err != nil {
		a.printErrf("msg list: %v\n", err)

		return ExitFailure
	}

	a.printIdentities(resp.GetIdentities())

	return ExitOK
}

// printIdentities renders the listing, saying so plainly when serve holds none
// rather than printing a bare header an operator has to interpret.
func (a *App) printIdentities(identities []*cairnosv1.Identity) {
	if len(identities) == 0 {
		a.printOutf("no messaging identities are configured\n")

		return
	}

	tab := newTab(a.stdout)

	a.tabWritef(tab, "NAME\tTRANSPORT\tUSERNAME\tDISPLAY\tURL\n")

	for _, identity := range identities {
		a.tabWritef(tab, "%s\t%s\t%s\t%s\t%s\n",
			identity.GetName(), identity.GetTransport(), identity.GetUsername(),
			identity.GetDisplayName(), identity.GetUrl())
	}

	a.flushTab(tab)
}

// warnPlaintextToken says so when a bearer token is about to cross a network.
// dialServe always uses insecure credentials, so an operator following the
// README's invocation against a remote serve puts their access token on the wire
// in the clear with nothing saying so.
func (a *App) warnPlaintextToken(addr, token string) {
	if token == "" || localAddr(addr) {
		return
	}

	a.printErrf("warning: --token is sent unencrypted to %s; put serve behind TLS or reach it over loopback\n", addr)
}

// localAddr reports whether addr stays on this host: a unix socket, or a loopback
// host with a port.
func localAddr(addr string) bool {
	if strings.HasPrefix(addr, unixScheme) {
		return true
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	if host == "" || host == "localhost" {
		return true
	}

	parsed := net.ParseIP(strings.Trim(host, "[]"))

	return parsed != nil && parsed.IsLoopback()
}

// dialServe opens the CLI's connection to a running serve. The credentials are
// insecure because every cairnos client dials loopback by default and serve
// terminates TLS ahead of the gRPC port when it is configured for it.
func (a *App) dialServe(addr string) (*grpc.ClientConn, error) {
	options := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if a.serveDialer != nil {
		options = append(options, grpc.WithContextDialer(a.serveDialer))
	}

	conn, err := grpc.NewClient(addr, options...)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	return conn, nil
}
