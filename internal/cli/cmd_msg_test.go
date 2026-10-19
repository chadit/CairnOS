package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/cli"
)

// Command, flag, and fixture literals the msg cases share, named so a typo
// fails in one place and goconst stays quiet.
const (
	msgCmd        = "msg"
	msgSendCmd    = "send"
	flagAsArg     = "--as"
	flagToArg     = "--to"
	flagThreadArg = "--thread"
	flagAddrArg   = "--addr"
	flagTokenArg  = "--token"

	// msgTestToken is the throwaway bearer the plaintext-warning cases pass.
	msgTestToken = "cairnos-msg-test-token"

	msgIdentity = "ops-bot"
	msgTarget   = "#general"
	msgPostID   = "p-9000"
	msgThreadID = "p-root"

	// msgBody is the throwaway message the cases that care about flags rather
	// than text all send.
	msgBody = "hello"
)

// fakeMessagingServer answers the two MessagingService calls from a script, and
// records the send requests so a case can prove which text and thread the CLI
// put on the wire. The mutex guards against the gRPC server's own goroutine.
type fakeMessagingServer struct {
	cairnosv1.UnimplementedMessagingServiceServer

	sendErr    error
	sends      []*cairnosv1.MessagingServiceSendMessageRequest
	identities []*cairnosv1.Identity
	mu         sync.Mutex
}

func (s *fakeMessagingServer) SendMessage(
	_ context.Context,
	req *cairnosv1.MessagingServiceSendMessageRequest,
) (*cairnosv1.MessagingServiceSendMessageResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sends = append(s.sends, req)

	if s.sendErr != nil {
		return nil, s.sendErr
	}

	return &cairnosv1.MessagingServiceSendMessageResponse{PostId: msgPostID, ChannelId: "c-1"}, nil
}

func (s *fakeMessagingServer) ListIdentities(
	_ context.Context,
	_ *cairnosv1.ListIdentitiesRequest,
) (*cairnosv1.ListIdentitiesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return &cairnosv1.ListIdentitiesResponse{Identities: s.identities}, nil
}

// lastSend returns the request the server saw most recently, failing the test
// when the CLI never made the call.
func (s *fakeMessagingServer) lastSend(t *testing.T) *cairnosv1.MessagingServiceSendMessageRequest {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.sends) == 0 {
		t.Fatal("the server saw no SendMessage call")
	}

	return s.sends[len(s.sends)-1]
}

// sendCount reports how many sends reached the server, so a rejected invocation
// can be shown never to have dialed.
func (s *fakeMessagingServer) sendCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.sends)
}

// startMessagingServer serves the fake in memory for an App to reach through
// its dial.
func startMessagingServer(t *testing.T, fake *fakeMessagingServer) *serveFixture {
	t.Helper()

	return serveInMemory(t, func(server *grpc.Server) {
		cairnosv1.RegisterMessagingServiceServer(server, fake)
	})
}

// runMsgCLI drives one msg invocation with the given stdin and returns the exit
// code and both streams.
func runMsgCLI(t *testing.T, msgServer *serveFixture, stdin io.Reader, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	app := cli.New(&stdout, &stderr).WithStdin(stdin)
	if msgServer != nil {
		app = app.WithServeDialer(msgServer.dial)
	}

	code := app.Run(t.Context(), args)

	return code, stdout.String(), stderr.String()
}

func TestMsgSendJoinsPositionalTextAndPrintsThePostID(t *testing.T) {
	t.Parallel()

	fake := &fakeMessagingServer{}
	msgServer := startMessagingServer(t, fake)

	code, stdout, stderr := runMsgCLI(t, msgServer, strings.NewReader(""),
		msgCmd, msgSendCmd, flagAddrArg, msgServer.addr, flagAsArg, msgIdentity, flagToArg, msgTarget,
		"deploy", "finished", "clean")

	if code != cli.ExitOK {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, cli.ExitOK, stderr)
	}

	if got := strings.TrimSpace(stdout); got != msgPostID {
		t.Errorf("stdout %q, want the post id %q", got, msgPostID)
	}

	sent := fake.lastSend(t)

	if sent.GetText() != "deploy finished clean" {
		t.Errorf("sent text %q, want the words joined by spaces", sent.GetText())
	}

	if sent.GetIdentity() != msgIdentity || sent.GetTo() != msgTarget {
		t.Errorf("sent identity %q to %q, want %q and %q",
			sent.GetIdentity(), sent.GetTo(), msgIdentity, msgTarget)
	}

	if sent.GetThreadId() != "" {
		t.Errorf("sent thread id %q with no --thread", sent.GetThreadId())
	}
}

func TestMsgSendReadsTextFromStdinOnDash(t *testing.T) {
	t.Parallel()

	fake := &fakeMessagingServer{}
	msgServer := startMessagingServer(t, fake)

	body := "line one\nline two\n"

	code, _, stderr := runMsgCLI(t, msgServer, strings.NewReader(body),
		msgCmd, msgSendCmd, flagAddrArg, msgServer.addr, flagAsArg, msgIdentity, flagToArg, msgTarget, "-")

	if code != cli.ExitOK {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, cli.ExitOK, stderr)
	}

	// One trailing newline comes from the pipeline, not the message, so the
	// posted body keeps the interior newline and drops only the last one.
	if got := fake.lastSend(t).GetText(); got != "line one\nline two" {
		t.Errorf("sent text %q, want the piped body without its trailing newline", got)
	}
}

func TestMsgSendPassesThreadThrough(t *testing.T) {
	t.Parallel()

	fake := &fakeMessagingServer{}
	msgServer := startMessagingServer(t, fake)

	code, _, stderr := runMsgCLI(t, msgServer, strings.NewReader(""),
		msgCmd, msgSendCmd, flagAddrArg, msgServer.addr, flagAsArg, msgIdentity, flagToArg, msgTarget,
		flagThreadArg, msgThreadID, "replying")

	if code != cli.ExitOK {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, cli.ExitOK, stderr)
	}

	if got := fake.lastSend(t).GetThreadId(); got != msgThreadID {
		t.Errorf("sent thread id %q, want %q", got, msgThreadID)
	}
}

func TestMsgSendReportsAServerRefusal(t *testing.T) {
	t.Parallel()

	fake := &fakeMessagingServer{
		sendErr: status.Error(codes.NotFound, fmt.Sprintf("no messaging identity named %q is configured", msgIdentity)),
	}
	msgServer := startMessagingServer(t, fake)

	code, stdout, stderr := runMsgCLI(t, msgServer, strings.NewReader(""),
		msgCmd, msgSendCmd, flagAddrArg, msgServer.addr, flagAsArg, msgIdentity, flagToArg, msgTarget, msgBody)

	if code != cli.ExitFailure {
		t.Fatalf("exit %d, want %d", code, cli.ExitFailure)
	}

	if stdout != "" {
		t.Errorf("stdout %q on a refused send, want nothing", stdout)
	}

	if !strings.Contains(stderr, msgIdentity) {
		t.Errorf("stderr %q does not name the identity the server rejected", stderr)
	}
}

func TestMsgSendRejectsBadInvocations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		stdin string
		args  []string
	}{
		{
			name: "missing --as",
			args: []string{flagToArg, msgTarget, msgBody},
		},
		{
			name: "missing --to",
			args: []string{flagAsArg, msgIdentity, msgBody},
		},
		{
			name: "no text",
			args: []string{flagAsArg, msgIdentity, flagToArg, msgTarget},
		},
		{
			name:  "empty stdin body",
			stdin: "   \n",
			args:  []string{flagAsArg, msgIdentity, flagToArg, msgTarget, "-"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeMessagingServer{}
			msgServer := startMessagingServer(t, fake)

			args := append([]string{msgCmd, msgSendCmd, flagAddrArg, msgServer.addr}, tt.args...)

			code, _, stderr := runMsgCLI(t, msgServer, strings.NewReader(tt.stdin), args...)

			if code != cli.ExitUsage {
				t.Fatalf("exit %d, want %d", code, cli.ExitUsage)
			}

			if stderr == "" {
				t.Error("a rejected invocation printed nothing to stderr")
			}

			if got := fake.sendCount(); got != 0 {
				t.Errorf("the server saw %d sends; a rejected invocation must not dial", got)
			}
		})
	}
}

func TestMsgListPrintsTheConfiguredIdentities(t *testing.T) {
	t.Parallel()

	const (
		listedUsername = "cairnos"
		listedURL      = "https://mm.example"
	)

	fake := &fakeMessagingServer{identities: []*cairnosv1.Identity{{
		Name:        msgIdentity,
		Transport:   transportMattermost,
		Username:    listedUsername,
		DisplayName: "CairnOS",
		Url:         listedURL,
	}}}
	msgServer := startMessagingServer(t, fake)

	code, stdout, stderr := runMsgCLI(t, msgServer, strings.NewReader(""),
		msgCmd, "list", flagAddrArg, msgServer.addr)

	if code != cli.ExitOK {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, cli.ExitOK, stderr)
	}

	for _, want := range []string{msgIdentity, transportMattermost, listedUsername, listedURL} {
		if !strings.Contains(stdout, want) {
			t.Errorf("listing %q is missing %q", stdout, want)
		}
	}
}

func TestMsgListSaysSoWhenNoIdentitiesAreConfigured(t *testing.T) {
	t.Parallel()

	msgServer := startMessagingServer(t, &fakeMessagingServer{})

	code, stdout, stderr := runMsgCLI(t, msgServer, strings.NewReader(""), msgCmd, "list", flagAddrArg, msgServer.addr)

	if code != cli.ExitOK {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, cli.ExitOK, stderr)
	}

	if !strings.Contains(stdout, "no messaging identities") {
		t.Errorf("stdout %q does not say the listing is empty", stdout)
	}
}

func TestMsgRejectsAnUnknownSubcommand(t *testing.T) {
	t.Parallel()

	code, _, stderr := runMsgCLI(t, nil, strings.NewReader(""), msgCmd, "broadcast")

	if code != cli.ExitUsage {
		t.Fatalf("exit %d, want %d", code, cli.ExitUsage)
	}

	if !strings.Contains(stderr, "broadcast") {
		t.Errorf("stderr %q does not name the unknown subcommand", stderr)
	}
}

// TestMsgSendWarnsAboutAPlaintextTokenOffLoopback proves a bearer token bound
// for another host is called out. dialServe always uses insecure credentials, so
// the invocation the README shows puts an access token on the wire in the clear
// against a remote serve, and nothing said so.
func TestMsgSendWarnsAboutAPlaintextTokenOffLoopback(t *testing.T) {
	t.Parallel()

	fake := &fakeMessagingServer{}
	msgServer := startMessagingServer(t, fake)

	// The fixture dialer answers whatever address it is given, so the
	// unspecified address reaches the same fixture while reading as a network
	// address, and the case exercises the warning without an unroutable host.
	_, port, err := net.SplitHostPort(msgServer.addr)
	if err != nil {
		t.Fatalf("split %q: %v", msgServer.addr, err)
	}

	code, _, stderr := runMsgCLI(t, msgServer, strings.NewReader(""),
		msgCmd, msgSendCmd, flagAddrArg, net.JoinHostPort("0.0.0.0", port),
		flagTokenArg, msgTestToken, flagAsArg, msgIdentity, flagToArg, msgTarget, msgBody)

	if code != cli.ExitOK {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, cli.ExitOK, stderr)
	}

	if !strings.Contains(stderr, "unencrypted") {
		t.Fatalf("stderr = %q, want a warning that the token crosses the network in the clear", stderr)
	}
}

// TestMsgSendStaysQuietWithATokenOnLoopback proves the warning is about the
// network rather than about passing a token at all.
func TestMsgSendStaysQuietWithATokenOnLoopback(t *testing.T) {
	t.Parallel()

	fake := &fakeMessagingServer{}
	msgServer := startMessagingServer(t, fake)

	code, _, stderr := runMsgCLI(t, msgServer, strings.NewReader(""),
		msgCmd, msgSendCmd, flagAddrArg, msgServer.addr, flagTokenArg, msgTestToken,
		flagAsArg, msgIdentity, flagToArg, msgTarget, msgBody)

	if code != cli.ExitOK {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, cli.ExitOK, stderr)
	}

	if strings.Contains(stderr, "unencrypted") {
		t.Fatalf("stderr = %q, want no warning for a loopback address", stderr)
	}
}
