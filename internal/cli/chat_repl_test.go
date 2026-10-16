package cli_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/cli"
)

// Shared fixture literals, named so goconst stays quiet and a typo fails in
// one place. Every test continues startSessionID via --session, so session
// creation observed by the scripted server always comes from a slash command.
const (
	chatCmd = "chat"

	startSessionID    = "sess-1"
	startPersonality  = "base"
	zenPersonality    = "zen"
	newSessionID      = "sess-new"
	switchedSessionID = "sess-2"

	// currentSessionEcho is the bare /session line; the trailing newline
	// keeps it distinct from the greeting, which continues with "; type...".
	currentSessionEcho = "session sess-1 (base)\n"

	searchQueryText = "hive"

	// pastSessionA and pastSessionB are sessions already on the server, the
	// ones search hits and session listings name.
	pastSessionA = "sess-a"
	pastSessionB = "sess-b"

	// Hit lines in the shared "session <id> #<seq> [role] <text>" format,
	// the output contract both /search and --search print in.
	searchHitUserLine      = "session sess-a #1 [user] deploy the hive"
	searchHitAssistantLine = "session sess-b #4 [assistant] hive deployed"
)

// searchHitFixtures are the canned hits the search tests serve, spanning two
// sessions and both roles so the shared hit format is fully exercised.
func searchHitFixtures() []*cairnosv1.ChatMessage {
	return []*cairnosv1.ChatMessage{
		{SessionId: pastSessionA, Seq: 1, Role: "user", Content: "deploy the hive"},
		{SessionId: pastSessionB, Seq: 4, Role: "assistant", Content: "hive deployed"},
	}
}

// scriptedChatServer is an in-test ChatService that serves canned responses
// and records what the CLI sends, so REPL tests drive the real gRPC client
// over loopback without a serve daemon.
type scriptedChatServer struct {
	cairnosv1.UnimplementedChatServiceServer

	sendErr     error
	sendStarted chan struct{}
	sessions    map[string]*cairnosv1.ChatSession
	createReqs  []*cairnosv1.CreateSessionRequest
	searchHits  []*cairnosv1.ChatMessage
	sendEvents  []*cairnosv1.ChatEvent
	listed      []*cairnosv1.ChatSession
	sendReqs    []*cairnosv1.SendMessageRequest
	searchReqs  []*cairnosv1.SearchMessagesRequest
	approvals   []bool
	mu          sync.Mutex
	blockSend   bool
}

// newScriptedChat seeds the starting session every test continues via
// --session.
func newScriptedChat() *scriptedChatServer {
	return &scriptedChatServer{sessions: map[string]*cairnosv1.ChatSession{
		startSessionID: {Id: startSessionID, Personality: startPersonality},
	}}
}

func (s *scriptedChatServer) CreateSession(_ context.Context, req *cairnosv1.CreateSessionRequest) (*cairnosv1.CreateSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.createReqs = append(s.createReqs, req)

	session := &cairnosv1.ChatSession{Id: newSessionID, Personality: req.GetPersonality()}

	return &cairnosv1.CreateSessionResponse{Session: session}, nil
}

func (s *scriptedChatServer) GetSession(_ context.Context, req *cairnosv1.GetSessionRequest) (*cairnosv1.GetSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	session, ok := s.sessions[req.GetSessionId()]
	if !ok {
		return nil, fmt.Errorf("scripted get session: %w", status.Error(codes.NotFound, "session not found"))
	}

	return &cairnosv1.GetSessionResponse{Session: session}, nil
}

func (s *scriptedChatServer) ListSessions(_ context.Context, _ *cairnosv1.ListSessionsRequest) (*cairnosv1.ListSessionsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return &cairnosv1.ListSessionsResponse{Sessions: s.listed}, nil
}

func (s *scriptedChatServer) SendMessage(req *cairnosv1.SendMessageRequest, stream grpc.ServerStreamingServer[cairnosv1.ChatEvent]) error {
	s.mu.Lock()

	s.sendReqs = append(s.sendReqs, req)
	block := s.blockSend
	events := s.sendEvents
	started := s.sendStarted
	failErr := s.sendErr
	// Only the first send signals or fails, so a later send streams normally.
	s.sendStarted = nil
	s.sendErr = nil

	s.mu.Unlock()

	if started != nil {
		close(started)
	}

	if failErr != nil {
		// Stand in for a transient stream failure (serve restart, network
		// blip): the request is recorded, then the turn dies mid-stream.
		return failErr
	}

	if block {
		// Stand in for a server mid-turn: hold the stream open until the
		// client cancels, which is exactly what an interrupt does.
		<-stream.Context().Done()

		return nil
	}

	for _, event := range events {
		if err := stream.Send(event); err != nil {
			return fmt.Errorf("scripted send: %w", err)
		}
	}

	return nil
}

func (s *scriptedChatServer) SearchMessages(_ context.Context, req *cairnosv1.SearchMessagesRequest) (*cairnosv1.SearchMessagesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.searchReqs = append(s.searchReqs, req)

	return &cairnosv1.SearchMessagesResponse{Messages: s.searchHits}, nil
}

func (s *scriptedChatServer) ConfirmAction(req *cairnosv1.ConfirmActionRequest, _ grpc.ServerStreamingServer[cairnosv1.ChatEvent]) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.approvals = append(s.approvals, req.GetApprove())

	return nil
}

// sentMessages snapshots the recorded SendMessage requests.
func (s *scriptedChatServer) sentMessages() []*cairnosv1.SendMessageRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]*cairnosv1.SendMessageRequest(nil), s.sendReqs...)
}

// createRequests snapshots the recorded CreateSession requests.
func (s *scriptedChatServer) createRequests() []*cairnosv1.CreateSessionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]*cairnosv1.CreateSessionRequest(nil), s.createReqs...)
}

// searchRequests snapshots the recorded SearchMessages requests.
func (s *scriptedChatServer) searchRequests() []*cairnosv1.SearchMessagesRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]*cairnosv1.SearchMessagesRequest(nil), s.searchReqs...)
}

// approvalValues snapshots the recorded ConfirmAction approve flags.
func (s *scriptedChatServer) approvalValues() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]bool(nil), s.approvals...)
}

// unblockSend lets the next SendMessage stream normally.
func (s *scriptedChatServer) unblockSend() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.blockSend = false
}

// startScriptedChat serves the scripted ChatService on an ephemeral loopback
// port and returns the address for --addr.
func startScriptedChat(t *testing.T, script *scriptedChatServer) string {
	t.Helper()

	listenConfig := net.ListenConfig{}

	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	server := grpc.NewServer()
	cairnosv1.RegisterChatServiceServer(server, script)

	served := make(chan error, 1)

	go func() { served <- server.Serve(listener) }()

	t.Cleanup(func() {
		server.Stop()

		if err := <-served; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("serve: %v", err)
		}
	})

	return listener.Addr().String()
}

// syncBuffer is a mutex-guarded buffer: the REPL's interrupt watcher prints
// from its own goroutine, so the interrupt tests need synchronized writes.
type syncBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (b *syncBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	count, err := b.buf.Write(data)
	if err != nil {
		return count, fmt.Errorf("buffer write: %w", err)
	}

	return count, nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// failingReader makes every read fail so the scanner surfaces an error.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("boom")
}

// chatArgs builds the chat invocation continuing the seeded starting session.
func chatArgs(addr string) []string {
	return []string{chatCmd, flagAddrArg, addr, "--session", startSessionID}
}

// runChatCLI drives one scripted conversation through the full chat command,
// continuing the seeded starting session. The injected interrupt channel
// never fires; it only keeps the command off the real signal machinery.
func runChatCLI(t *testing.T, addr string, stdin io.Reader) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	app := cli.New(&stdout, &stderr).WithStdin(stdin).WithInterrupts(make(chan os.Signal))
	code := app.Run(t.Context(), chatArgs(addr))

	return code, stdout.String(), stderr.String()
}

// writeLine feeds one line into the REPL's stdin pipe; the write returns only
// after the REPL has read it, which sequences the test without polling.
func writeLine(t *testing.T, writer *io.PipeWriter, line string) {
	t.Helper()

	if _, err := io.WriteString(writer, line+"\n"); err != nil {
		t.Fatalf("write %q: %v", line, err)
	}
}

func TestChatSlashCommands(t *testing.T) {
	t.Parallel()

	updated := timestamppb.New(time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC))

	tests := []struct {
		name       string
		stdin      string
		configure  func(script *scriptedChatServer)
		wantOut    []string
		wantErrOut []string
	}{
		{
			name:    "help shows the commands and the immutability note",
			stdin:   "/help\n/quit\n",
			wantOut: []string{"/new [personality]", "immutable per session", "/search <text>"},
		},
		{
			name:    "session bare prints the current session",
			stdin:   "/session\n/quit\n",
			wantOut: []string{currentSessionEcho},
		},
		{
			name:  "session list prints sessions in arrival order",
			stdin: "/session list\n/quit\n",
			configure: func(script *scriptedChatServer) {
				script.listed = []*cairnosv1.ChatSession{
					{Id: pastSessionB, Personality: zenPersonality, UpdatedAt: updated},
					{Id: pastSessionA, Personality: startPersonality, UpdatedAt: updated},
				}
			},
			wantOut: []string{
				"session sess-b (zen) updated 2026-07-01T12:00:00Z",
				"session sess-a (base)",
			},
		},
		{
			name:  "session switch adopts the named session",
			stdin: "/session " + switchedSessionID + "\n/quit\n",
			configure: func(script *scriptedChatServer) {
				script.sessions[switchedSessionID] = &cairnosv1.ChatSession{Id: switchedSessionID, Personality: zenPersonality}
			},
			wantOut: []string{"session sess-2 (zen)\n"},
		},
		{
			name:       "session switch to a missing id stays put",
			stdin:      "/session nope\n/session\n/quit\n",
			wantOut:    []string{currentSessionEcho},
			wantErrOut: []string{"get session nope", "session not found"},
		},
		{
			name:    "personality bare prints the current personality",
			stdin:   "/personality\n/quit\n",
			wantOut: []string{"personality base\n"},
		},
		{
			name:    "unknown command prints the help hint",
			stdin:   "/bogus\n/quit\n",
			wantOut: []string{"unknown command, try /help"},
		},
		{
			name:    "search bare prints its usage",
			stdin:   "/search\n/quit\n",
			wantOut: []string{"usage: /search <text>"},
		},
		{
			name:    "exit leaves like quit",
			stdin:   "/exit\n",
			wantOut: []string{"> "},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			script := newScriptedChat()
			if tt.configure != nil {
				tt.configure(script)
			}

			addr := startScriptedChat(t, script)

			code, stdout, stderr := runChatCLI(t, addr, strings.NewReader(tt.stdin))
			if code != cli.ExitOK {
				t.Fatalf("exit = %d, want %d; stderr: %s", code, cli.ExitOK, stderr)
			}

			for _, want := range tt.wantOut {
				if !strings.Contains(stdout, want) {
					t.Errorf("stdout missing %q:\n%s", want, stdout)
				}
			}

			for _, want := range tt.wantErrOut {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr missing %q:\n%s", want, stderr)
				}
			}
		})
	}
}

func TestChatNewSessionRoutesFollowingMessages(t *testing.T) {
	t.Parallel()

	script := newScriptedChat()
	addr := startScriptedChat(t, script)

	code, stdout, stderr := runChatCLI(t, addr, strings.NewReader("/new poet\nhello there\n/quit\n"))
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, cli.ExitOK, stderr)
	}

	if !strings.Contains(stdout, "session sess-new (poet)") {
		t.Errorf("stdout missing the switch echo:\n%s", stdout)
	}

	creates := script.createRequests()
	if len(creates) != 1 || creates[0].GetPersonality() != "poet" {
		t.Fatalf("create requests = %v, want one with personality poet", creates)
	}

	sent := script.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sent))
	}

	if sent[0].GetSessionId() != newSessionID {
		t.Errorf("message went to %q, want %q", sent[0].GetSessionId(), newSessionID)
	}
}

func TestChatPersonalityStartsNewSession(t *testing.T) {
	t.Parallel()

	script := newScriptedChat()
	addr := startScriptedChat(t, script)

	code, stdout, stderr := runChatCLI(t, addr, strings.NewReader("/personality pirate\n/quit\n"))
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, cli.ExitOK, stderr)
	}

	if !strings.Contains(stdout, "session sess-new (pirate)") {
		t.Errorf("stdout missing the switch echo:\n%s", stdout)
	}

	creates := script.createRequests()
	if len(creates) != 1 || creates[0].GetPersonality() != "pirate" {
		t.Fatalf("create requests = %v, want one with personality pirate", creates)
	}
}

func TestChatSessionSwitchRoutesFollowingMessages(t *testing.T) {
	t.Parallel()

	script := newScriptedChat()
	script.sessions[switchedSessionID] = &cairnosv1.ChatSession{Id: switchedSessionID, Personality: zenPersonality}
	addr := startScriptedChat(t, script)

	stdin := "first message\n/session " + switchedSessionID + "\nsecond message\n/quit\n"

	code, _, stderr := runChatCLI(t, addr, strings.NewReader(stdin))
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, cli.ExitOK, stderr)
	}

	sent := script.sentMessages()
	if len(sent) != 2 {
		t.Fatalf("sent %d messages, want 2", len(sent))
	}

	if sent[0].GetSessionId() != startSessionID {
		t.Errorf("first message went to %q, want %q", sent[0].GetSessionId(), startSessionID)
	}

	if sent[1].GetSessionId() != switchedSessionID {
		t.Errorf("second message went to %q, want %q", sent[1].GetSessionId(), switchedSessionID)
	}
}

func TestChatMultilineJoinsIntoOneMessage(t *testing.T) {
	t.Parallel()

	script := newScriptedChat()
	addr := startScriptedChat(t, script)

	code, stdout, stderr := runChatCLI(t, addr, strings.NewReader("first line \\\nsecond line \\\nthird line\n/quit\n"))
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, cli.ExitOK, stderr)
	}

	if !strings.Contains(stdout, "... ") {
		t.Errorf("continuation prompt missing:\n%s", stdout)
	}

	sent := script.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sent))
	}

	want := "first line \nsecond line \nthird line"
	if sent[0].GetMessage() != want {
		t.Errorf("message = %q, want %q", sent[0].GetMessage(), want)
	}
}

func TestChatEOFMidBlockSendsNothing(t *testing.T) {
	t.Parallel()

	script := newScriptedChat()
	addr := startScriptedChat(t, script)

	code, _, stderr := runChatCLI(t, addr, strings.NewReader("dangling \\\n"))
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, cli.ExitOK, stderr)
	}

	if sent := script.sentMessages(); len(sent) != 0 {
		t.Errorf("sent %d messages, want none", len(sent))
	}
}

func TestChatCleanEOFExitsZero(t *testing.T) {
	t.Parallel()

	addr := startScriptedChat(t, newScriptedChat())

	code, _, stderr := runChatCLI(t, addr, strings.NewReader(""))
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, cli.ExitOK, stderr)
	}
}

func TestChatScannerErrorExitsNonZero(t *testing.T) {
	t.Parallel()

	addr := startScriptedChat(t, newScriptedChat())

	code, _, stderr := runChatCLI(t, addr, failingReader{})
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want %d", code, cli.ExitFailure)
	}

	if !strings.Contains(stderr, "read input") {
		t.Errorf("stderr missing the read error:\n%s", stderr)
	}
}

// TestChatTurnErrorKeepsREPLAlive pins the spec's exit contract: the process
// only exits on /quit, /exit, stdin EOF, or a scanner error, so a failed turn
// prints the error and offers the next prompt instead of killing the REPL.
func TestChatTurnErrorKeepsREPLAlive(t *testing.T) {
	t.Parallel()

	script := newScriptedChat()
	script.sendErr = status.Error(codes.Unavailable, "serve restarting")
	addr := startScriptedChat(t, script)

	code, _, stderr := runChatCLI(t, addr, strings.NewReader("first try\nsecond try\n/quit\n"))
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, cli.ExitOK, stderr)
	}

	if !strings.Contains(stderr, "serve restarting") {
		t.Errorf("stderr missing the turn error:\n%s", stderr)
	}

	sent := script.sentMessages()
	if len(sent) != 2 {
		t.Fatalf("sent %d messages, want 2; the loop should survive the failed first turn", len(sent))
	}

	if sent[1].GetMessage() != "second try" {
		t.Errorf("second message = %q, want %q", sent[1].GetMessage(), "second try")
	}
}

// TestChatApprovalReadsThroughSharedScanner is the regression test for the
// double-scanner bug: the whole script is buffered up front, and the old code
// built a second scanner that could never see the "y" the first scanner had
// already pulled into its buffer.
func TestChatApprovalReadsThroughSharedScanner(t *testing.T) {
	t.Parallel()

	script := newScriptedChat()
	script.sendEvents = []*cairnosv1.ChatEvent{{
		Type:     cairnosv1.ChatEventType_CHAT_EVENT_TYPE_CONFIRMATION_REQUIRED,
		Proposal: &cairnosv1.WorkflowProposal{WorkflowName: "deploy", EstimatedBudgetUsd: 0.25},
	}}
	addr := startScriptedChat(t, script)

	code, stdout, stderr := runChatCLI(t, addr, strings.NewReader("run the deploy\ny\n/quit\n"))
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, cli.ExitOK, stderr)
	}

	if !strings.Contains(stdout, `Run workflow "deploy"`) {
		t.Errorf("stdout missing the approval prompt:\n%s", stdout)
	}

	approvals := script.approvalValues()
	if len(approvals) != 1 || !approvals[0] {
		t.Fatalf("approvals = %v, want [true]", approvals)
	}
}

func TestChatInterruptMidTurnCancelsOnlyThatTurn(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	script := newScriptedChat()
	script.blockSend = true
	script.sendStarted = started
	addr := startScriptedChat(t, script)

	stdinReader, stdinWriter := io.Pipe()
	stdout := &syncBuffer{}
	stderr := &syncBuffer{}
	interrupts := make(chan os.Signal, 1)

	app := cli.New(stdout, stderr).WithStdin(stdinReader).WithInterrupts(interrupts)

	exit := make(chan int, 1)

	go func() {
		exit <- app.Run(t.Context(), chatArgs(addr))
	}()

	writeLine(t, stdinWriter, "hang forever")

	// Fire the interrupt only once the server holds the stream open, so it
	// lands mid-turn by construction.
	<-started

	interrupts <- os.Interrupt

	// The next writes only complete after the REPL is back at the prompt,
	// which proves the process and session survived the interrupt.
	script.unblockSend()
	writeLine(t, stdinWriter, "still here")
	writeLine(t, stdinWriter, "/quit")

	if code := <-exit; code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, cli.ExitOK, stderr.String())
	}

	if !strings.Contains(stdout.String(), "turn interrupted; the last message is saved without a reply") {
		t.Errorf("stdout missing the interrupt notice:\n%s", stdout.String())
	}

	sent := script.sentMessages()
	if len(sent) != 2 {
		t.Fatalf("sent %d messages, want 2", len(sent))
	}

	for _, req := range sent {
		if req.GetSessionId() != startSessionID {
			t.Errorf("message went to %q, want %q", req.GetSessionId(), startSessionID)
		}
	}
}

func TestChatInterruptAtPromptPrintsQuitHint(t *testing.T) {
	t.Parallel()

	addr := startScriptedChat(t, newScriptedChat())

	stdinReader, stdinWriter := io.Pipe()
	stdout := &syncBuffer{}
	stderr := &syncBuffer{}

	// Unbuffered: the send below returns only once the REPL's watcher has
	// taken the signal, so the hint is committed before /quit goes in.
	interrupts := make(chan os.Signal)

	app := cli.New(stdout, stderr).WithStdin(stdinReader).WithInterrupts(interrupts)

	exit := make(chan int, 1)

	go func() {
		exit <- app.Run(t.Context(), chatArgs(addr))
	}()

	interrupts <- os.Interrupt

	writeLine(t, stdinWriter, "/quit")

	if code := <-exit; code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, cli.ExitOK, stderr.String())
	}

	if !strings.Contains(stdout.String(), "interrupt: type /quit to exit") {
		t.Errorf("stdout missing the quit hint:\n%s", stdout.String())
	}
}

// runChatArgs drives one invocation with explicit args over empty stdin, for
// the one-shot --search paths that never read a line.
func runChatArgs(t *testing.T, args []string) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	app := cli.New(&stdout, &stderr).WithStdin(strings.NewReader("")).WithInterrupts(make(chan os.Signal))
	code := app.Run(t.Context(), args)

	return code, stdout.String(), stderr.String()
}

// searchArgs builds the one-shot search invocation, plus any extra flags.
func searchArgs(addr, query string, extra ...string) []string {
	return append([]string{chatCmd, flagAddrArg, addr, "--search", query}, extra...)
}

// requireOneSearch asserts exactly one search request was recorded and
// returns it.
func requireOneSearch(t *testing.T, script *scriptedChatServer) *cairnosv1.SearchMessagesRequest {
	t.Helper()

	searches := script.searchRequests()
	if len(searches) != 1 {
		t.Fatalf("search requests = %d, want 1", len(searches))
	}

	return searches[0]
}

func TestChatSearchCommandQueriesWholeWorkspace(t *testing.T) {
	t.Parallel()

	script := newScriptedChat()
	script.searchHits = searchHitFixtures()
	addr := startScriptedChat(t, script)

	code, stdout, stderr := runChatCLI(t, addr, strings.NewReader("/search hive time\n/quit\n"))
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, cli.ExitOK, stderr)
	}

	for _, want := range []string{searchHitUserLine, searchHitAssistantLine} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}

	// Workspace-wide with the default cap: /search deliberately sets no
	// session filter, because finding the right session is the point.
	got := requireOneSearch(t, script)
	if got.GetText() != "hive time" || got.GetSessionId() != "" || got.GetLimit() != 20 {
		t.Errorf("search request = %v, want text %q, no session, limit 20", got, "hive time")
	}
}

func TestChatSearchFlagPrintsHitsWithoutREPL(t *testing.T) {
	t.Parallel()

	script := newScriptedChat()
	script.searchHits = searchHitFixtures()
	addr := startScriptedChat(t, script)

	code, stdout, stderr := runChatArgs(t, searchArgs(addr, searchQueryText))
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, cli.ExitOK, stderr)
	}

	for _, want := range []string{searchHitUserLine, searchHitAssistantLine} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}

	// No greeting and no prompt: the one-shot never enters the REPL, and it
	// never opens or resolves a session either.
	if strings.Contains(stdout, "type a message") || strings.Contains(stdout, "> ") {
		t.Errorf("one-shot search entered the REPL:\n%s", stdout)
	}

	if creates := script.createRequests(); len(creates) != 0 {
		t.Errorf("create requests = %d, want none", len(creates))
	}

	got := requireOneSearch(t, script)
	if got.GetText() != searchQueryText || got.GetSessionId() != "" || got.GetLimit() != 20 {
		t.Errorf("search request = %v, want text %q, no session, default limit 20", got, searchQueryText)
	}
}

func TestChatSearchFlagPassesSessionAndLimit(t *testing.T) {
	t.Parallel()

	script := newScriptedChat()
	addr := startScriptedChat(t, script)

	code, _, stderr := runChatArgs(t, searchArgs(addr, searchQueryText, "--session", startSessionID, "--limit", "5"))
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, cli.ExitOK, stderr)
	}

	got := requireOneSearch(t, script)
	if got.GetSessionId() != startSessionID || got.GetLimit() != 5 {
		t.Errorf("search request = %v, want session %q and limit 5", got, startSessionID)
	}
}

func TestChatSearchFlagEmptyFailsWithUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		query string
	}{
		{name: "empty string", query: ""},
		{name: "whitespace only", query: "   "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			script := newScriptedChat()
			addr := startScriptedChat(t, script)

			code, _, stderr := runChatArgs(t, searchArgs(addr, tt.query))
			if code != cli.ExitUsage {
				t.Fatalf("exit = %d, want %d; stderr: %s", code, cli.ExitUsage, stderr)
			}

			if !strings.Contains(stderr, "usage: cairnos chat") {
				t.Errorf("stderr missing the usage line:\n%s", stderr)
			}

			// The empty query must fail during flag parsing, before any RPC.
			if searches := script.searchRequests(); len(searches) != 0 {
				t.Errorf("search requests = %d, want none", len(searches))
			}
		})
	}
}
