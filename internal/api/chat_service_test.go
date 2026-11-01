package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/chadit/CairnOS/internal/api"
	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/nettest"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Chat inputs the tests send: a message that asks for the hello workflow, and
// search text whose only job is to reach the store.
const (
	msgRunHello   = "run hello"
	anySearchText = "anything"
)

// chatHelloTOML is the mock workflow a confirmed chat proposal fires.
const chatHelloTOML = `
[workflow]
name = "hello"

[action.greet]
runner = "mock"
params = { output = "hi" }
`

// scriptedChatDriver is an in-test ToolCaller that serves a canned sequence of
// turns, so the chat agent's behavior is reproducible without a real LLM.
type scriptedChatDriver struct {
	turns []runner.Result
	calls int
	mu    sync.Mutex
}

var (
	_ runner.Runner     = (*scriptedChatDriver)(nil)
	_ runner.ToolCaller = (*scriptedChatDriver)(nil)
)

func (*scriptedChatDriver) Name() string { return "scripted" }

func (*scriptedChatDriver) Validate(_ *runner.Task) error { return nil }

func (*scriptedChatDriver) Execute(_ context.Context, _ *runner.Task) (runner.Result, error) {
	return runner.Result{}, errScriptedExecute
}

func (d *scriptedChatDriver) ExecuteWithTools(_ context.Context, _ *runner.Task, _ []runner.ToolSpec) (runner.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	idx := min(d.calls, len(d.turns)-1)
	d.calls++

	return d.turns[idx], nil
}

// errScriptedExecute guards the scripted driver's plain Execute path.
var errScriptedExecute = errors.New("scripted chat driver only runs through ExecuteWithTools")

// chatFixture is a running server with a chat agent wired over a scripted
// driver, plus the gRPC client and HTTP base for the gateway.
type chatFixture struct {
	chat      cairnosv1.ChatServiceClient
	transport *nettest.Network
	httpBase  string
}

// startChatServer boots a server whose chat agent uses the given scripted
// driver. When auth is non-nil, the server enforces tokens; chatAuth wires a
// matching authenticator so scope tests can present real tokens.
func startChatServer(t *testing.T, driver *scriptedChatDriver) *chatFixture {
	t.Helper()

	dir := t.TempDir()
	workflowsDir := filepath.Join(dir, "workflows")

	if err := os.MkdirAll(workflowsDir, 0o750); err != nil {
		t.Fatalf("create workflows dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(workflowsDir, "hello.toml"), []byte(chatHelloTOML), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	db := openChatStore(t, dir)
	transport := nettest.NewNetwork()
	manager := api.NewManager(slog.New(slog.DiscardHandler))
	manager.Bind(engine.New(testEngineConfig(manager), testRegistry(t), db))

	tools := chat.BuildToolSet(nil)

	agent := chat.New(&chat.Config{
		Store:         db,
		Driver:        driver,
		Tools:         tools.Tools,
		Specs:         tools.Specs,
		Personalities: chat.NewPersonalities(nil, ""),
		Trigger:       api.NewChatWorkflowTrigger(manager, api.NewWorkflowResolver(workflowsDir, slog.New(slog.DiscardHandler))),
		NewID:         sqlite.NewID,
	})

	server := api.NewServer(&api.Config{
		Transport:    transport,
		GRPCAddr:     loopbackEphemeral,
		HTTPAddr:     loopbackEphemeral,
		WorkflowsDir: workflowsDir,
		Store:        db,
		Manager:      manager,
		Chat:         agent,
		Logger:       slog.New(slog.DiscardHandler),
	})

	startAndCleanup(t, server)

	conn := transport.DialGRPC(t, server.GRPCAddr())

	return &chatFixture{
		chat:      cairnosv1.NewChatServiceClient(conn),
		transport: transport,
		httpBase:  "http://" + server.HTTPAddr(),
	}
}

// openChatStore opens a sqlite store sharing one file for runs and chat.
func openChatStore(t *testing.T, dir string) *sqlite.Store {
	t.Helper()

	db, err := sqlite.Open(filepath.Join(dir, "cairnos.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

// startAndCleanup starts the server and registers shutdown.
func startAndCleanup(t *testing.T, server *api.Server) {
	t.Helper()

	if err := server.Start(t.Context()); err != nil {
		t.Fatalf("start server: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), shutdownTimeout)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("shutdown server: %v", err)
		}
	})
}

// finalTurn builds a driver turn answering with text.
func finalTurn(answer string) runner.Result {
	return runner.Result{
		Output: answer,
		Meta:   map[string]string{runner.MetaStopReason: runner.StopReasonEndTurn},
	}
}

// runWorkflowTurn builds a driver turn proposing the hello workflow.
func runWorkflowTurn() runner.Result {
	return runner.Result{
		Output: map[string]any{
			runner.ToolCallName:      chat.RunWorkflowToolName,
			runner.ToolCallArguments: map[string]any{"workflow": helloName},
		},
		Meta: map[string]string{runner.MetaStopReason: runner.StopReasonToolUse},
	}
}

// TestChatSendMessageStreamsAnswerOverGRPC proves a plain turn streams an
// assistant chunk then turn-done in order over gRPC.
func TestChatSendMessageStreamsAnswerOverGRPC(t *testing.T) {
	t.Parallel()

	fix := startChatServer(t, &scriptedChatDriver{turns: []runner.Result{finalTurn("hello back")}})
	ctx := t.Context()

	session, err := fix.chat.CreateSession(ctx, &cairnosv1.CreateSessionRequest{})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	stream, err := fix.chat.SendMessage(ctx, &cairnosv1.SendMessageRequest{
		SessionId: session.GetSession().GetId(),
		Message:   "hello",
	})
	if err != nil {
		t.Fatalf("send message: %v", err)
	}

	types := collectChatTypes(t, stream)

	want := []cairnosv1.ChatEventType{
		cairnosv1.ChatEventType_CHAT_EVENT_TYPE_ASSISTANT_CHUNK,
		cairnosv1.ChatEventType_CHAT_EVENT_TYPE_TURN_DONE,
	}
	if !chatTypesEqual(types, want) {
		t.Fatalf("event types = %v, want %v", types, want)
	}
}

// TestChatConfirmationGateStreamsRunOverGRPC proves the D9 gate over gRPC: a
// proposal streams confirmation-required, then an approve streams run-started,
// run-finished, turn-done in order, and no run fires before the approve.
func TestChatConfirmationGateStreamsRunOverGRPC(t *testing.T) {
	t.Parallel()

	fix := startChatServer(t, &scriptedChatDriver{turns: []runner.Result{runWorkflowTurn()}})
	ctx := t.Context()

	session, err := fix.chat.CreateSession(ctx, &cairnosv1.CreateSessionRequest{})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	sendStream, err := fix.chat.SendMessage(ctx, &cairnosv1.SendMessageRequest{
		SessionId: session.GetSession().GetId(),
		Message:   msgRunHello,
	})
	if err != nil {
		t.Fatalf("send message: %v", err)
	}

	sendTypes := collectChatTypes(t, sendStream)

	wantSend := []cairnosv1.ChatEventType{
		cairnosv1.ChatEventType_CHAT_EVENT_TYPE_CONFIRMATION_REQUIRED,
		cairnosv1.ChatEventType_CHAT_EVENT_TYPE_TURN_DONE,
	}
	if !chatTypesEqual(sendTypes, wantSend) {
		t.Fatalf("send event types = %v, want a confirmation gate then turn_done", sendTypes)
	}

	confirmStream, err := fix.chat.ConfirmAction(ctx, &cairnosv1.ConfirmActionRequest{
		SessionId: session.GetSession().GetId(),
		Approve:   true,
	})
	if err != nil {
		t.Fatalf("confirm action: %v", err)
	}

	confirmTypes := collectChatTypes(t, confirmStream)

	wantConfirm := []cairnosv1.ChatEventType{
		cairnosv1.ChatEventType_CHAT_EVENT_TYPE_RUN_STARTED,
		cairnosv1.ChatEventType_CHAT_EVENT_TYPE_RUN_FINISHED,
		cairnosv1.ChatEventType_CHAT_EVENT_TYPE_TURN_DONE,
	}
	if !chatTypesEqual(confirmTypes, wantConfirm) {
		t.Fatalf("confirm event types = %v, want run_started, run_finished, turn_done", confirmTypes)
	}
}

// TestChatSendMessageStreamsOverGateway proves the turn streams as
// newline-delimited JSON over the HTTP gateway, the same shape RunWorkflow uses.
func TestChatSendMessageStreamsOverGateway(t *testing.T) {
	t.Parallel()

	fix := startChatServer(t, &scriptedChatDriver{turns: []runner.Result{finalTurn("gateway hi")}})
	ctx := t.Context()

	session, err := fix.chat.CreateSession(ctx, &cairnosv1.CreateSessionRequest{})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	url := fix.httpBase + "/v1/chat/sessions/" + session.GetSession().GetId() + "/messages"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"message":"hi"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := fix.transport.Client().Do(req)
	if err != nil {
		t.Fatalf("post message: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	lines := readNDJSONLines(t, resp.Body)
	if len(lines) < 2 {
		t.Fatalf("gateway streamed %d lines, want at least 2 (chunk + done): %v", len(lines), lines)
	}

	if !strings.Contains(lines[0], "gateway hi") {
		t.Errorf("first gateway line = %q, want the assistant text", lines[0])
	}
}

// collectChatTypes drains a chat event stream to EOF, returning the event types
// in order.
func collectChatTypes(t *testing.T, stream grpc.ServerStreamingClient[cairnosv1.ChatEvent]) []cairnosv1.ChatEventType {
	t.Helper()

	var types []cairnosv1.ChatEventType

	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return types
		}

		if err != nil {
			t.Fatalf("recv chat event: %v", err)
		}

		types = append(types, event.GetType())
	}
}

// chatTypesEqual compares two event-type slices.
func chatTypesEqual(got, want []cairnosv1.ChatEventType) bool {
	if len(got) != len(want) {
		return false
	}

	for idx := range got {
		if got[idx] != want[idx] {
			return false
		}
	}

	return true
}

// readNDJSONLines reads the gateway stream body into its JSON lines.
func readNDJSONLines(t *testing.T, body io.Reader) []string {
	t.Helper()

	var lines []string

	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("read gateway stream: %v", err)
	}

	return lines
}

// wantGRPCCode asserts an error carries the given gRPC status code.
func wantGRPCCode(t *testing.T, err error, want codes.Code) {
	t.Helper()

	if status.Code(err) != want {
		t.Fatalf("status code = %v, want %v (err: %v)", status.Code(err), want, err)
	}
}

// drainChatStream reads a chat stream to EOF, returning the first non-EOF
// error so a scope test sees the stream's terminal status.
func drainChatStream(stream grpc.ServerStreamingClient[cairnosv1.ChatEvent]) error {
	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return err
		}
	}
}

// TestChatScopeEnforced proves the Phase 9 scope rules over the auth fixture: a
// token without chat:session is refused at SendMessage, a chat:session token
// without write:workflow holds a conversation but its approve is refused at
// ConfirmAction, and a token with both can confirm a workflow trigger (D9).
func TestChatScopeEnforced(t *testing.T) {
	t.Parallel()

	fix := startAuthServer(t)

	t.Run("no chat scope is refused", func(t *testing.T) {
		t.Parallel()

		ctx := authedCtx(t, fix.token(t, auth.ScopeReadRun))

		_, err := fix.chat.CreateSession(ctx, &cairnosv1.CreateSessionRequest{})
		wantGRPCCode(t, err, codes.PermissionDenied)
	})

	t.Run("chat scope without write:workflow cannot confirm a trigger", func(t *testing.T) {
		t.Parallel()

		ctx := authedCtx(t, fix.token(t, auth.ScopeChatSession))
		sessionID := openChatSession(t, fix, ctx)

		drainSendUntilGate(t, fix, ctx, sessionID)

		stream, err := fix.chat.ConfirmAction(ctx, &cairnosv1.ConfirmActionRequest{SessionId: sessionID, Approve: true})
		if err != nil {
			t.Fatalf("open confirm stream: %v", err)
		}

		wantGRPCCode(t, drainChatStream(stream), codes.PermissionDenied)
	})

	t.Run("chat plus write:workflow can confirm", func(t *testing.T) {
		t.Parallel()

		ctx := authedCtx(t, fix.token(t, auth.ScopeChatSession, auth.ScopeWriteWorkflow))
		sessionID := openChatSession(t, fix, ctx)

		drainSendUntilGate(t, fix, ctx, sessionID)

		stream, err := fix.chat.ConfirmAction(ctx, &cairnosv1.ConfirmActionRequest{SessionId: sessionID, Approve: true})
		if err != nil {
			t.Fatalf("open confirm stream: %v", err)
		}

		if err := drainChatStream(stream); err != nil {
			t.Fatalf("confirm with both scopes should succeed, got: %v", err)
		}
	})
}

// openChatSession creates a session under the call context's token.
//
//nolint:revive // t leads for t.Helper; ctx follows to carry the call's token
func openChatSession(t *testing.T, fix *authFixture, ctx context.Context) string {
	t.Helper()

	session, err := fix.chat.CreateSession(ctx, &cairnosv1.CreateSessionRequest{})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	return session.GetSession().GetId()
}

// drainSendUntilGate sends one message and drains the turn, leaving the session
// with a pending proposal (the fixture driver always proposes the workflow).
//
//nolint:revive // t leads for t.Helper; ctx follows to carry the call's token
func drainSendUntilGate(t *testing.T, fix *authFixture, ctx context.Context, sessionID string) {
	t.Helper()

	stream, err := fix.chat.SendMessage(ctx, &cairnosv1.SendMessageRequest{SessionId: sessionID, Message: msgRunHello})
	if err != nil {
		t.Fatalf("send message: %v", err)
	}

	if err := drainChatStream(stream); err != nil {
		t.Fatalf("drain send stream: %v", err)
	}
}

// completeChatTurn sends one message and drains the turn to EOF, so both the
// user message and the assistant reply are persisted before the caller
// searches for them.
func completeChatTurn(t *testing.T, client cairnosv1.ChatServiceClient, sessionID, message string) {
	t.Helper()

	stream, err := client.SendMessage(t.Context(), &cairnosv1.SendMessageRequest{SessionId: sessionID, Message: message})
	if err != nil {
		t.Fatalf("send message: %v", err)
	}

	if err := drainChatStream(stream); err != nil {
		t.Fatalf("drain send stream: %v", err)
	}
}

// TestChatSearchMessagesOverGRPC proves a completed turn's user and assistant
// messages both answer a full-text query, and every hit names its session so
// a caller can jump back into the conversation it came from.
func TestChatSearchMessagesOverGRPC(t *testing.T) {
	t.Parallel()

	fix := startChatServer(t, &scriptedChatDriver{turns: []runner.Result{finalTurn("the apollo program landed in 1969")}})
	ctx := t.Context()

	session, err := fix.chat.CreateSession(ctx, &cairnosv1.CreateSessionRequest{})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	sessionID := session.GetSession().GetId()
	completeChatTurn(t, fix.chat, sessionID, "tell me about apollo")

	resp, err := fix.chat.SearchMessages(ctx, &cairnosv1.SearchMessagesRequest{Text: "apollo"})
	if err != nil {
		t.Fatalf("search messages: %v", err)
	}

	roles := make(map[string]bool, len(resp.GetMessages()))

	for _, hit := range resp.GetMessages() {
		if hit.GetSessionId() != sessionID {
			t.Errorf("hit session_id = %q, want %q", hit.GetSessionId(), sessionID)
		}

		roles[hit.GetRole()] = true
	}

	if len(resp.GetMessages()) != 2 || !roles["user"] || !roles["assistant"] {
		t.Fatalf("search returned %d hits with roles %v, want the user and assistant messages",
			len(resp.GetMessages()), roles)
	}
}

// TestChatSearchMessagesOverGateway proves the search RPC answers on its GET
// route with the hit and its session id in the JSON body.
func TestChatSearchMessagesOverGateway(t *testing.T) {
	t.Parallel()

	fix := startChatServer(t, &scriptedChatDriver{turns: []runner.Result{finalTurn("the gateway found the beacon")}})

	session, err := fix.chat.CreateSession(t.Context(), &cairnosv1.CreateSessionRequest{})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	sessionID := session.GetSession().GetId()
	completeChatTurn(t, fix.chat, sessionID, "where is the beacon")

	code, body := httpGet(t, fix.transport, fix.httpBase+"/v1/chat/search?text=beacon")
	if code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", code, body)
	}

	if !strings.Contains(string(body), "beacon") || !strings.Contains(string(body), sessionID) {
		t.Fatalf("gateway body = %s, want a hit carrying the query text and its session id", body)
	}
}

// TestChatSearchScopeEnforced proves SearchMessages sits under chat:session
// in the method table: a token without the scope is refused, one with it
// answers. The success leg guards against the table's fail-closed default
// silently demanding admin for a registered chat method.
func TestChatSearchScopeEnforced(t *testing.T) {
	t.Parallel()

	fix := startAuthServer(t)

	t.Run("no chat scope is refused", func(t *testing.T) {
		t.Parallel()

		ctx := authedCtx(t, fix.token(t, auth.ScopeReadRun))

		_, err := fix.chat.SearchMessages(ctx, &cairnosv1.SearchMessagesRequest{Text: anySearchText})
		wantGRPCCode(t, err, codes.PermissionDenied)
	})

	t.Run("chat scope answers", func(t *testing.T) {
		t.Parallel()

		ctx := authedCtx(t, fix.token(t, auth.ScopeChatSession))

		if _, err := fix.chat.SearchMessages(ctx, &cairnosv1.SearchMessagesRequest{Text: anySearchText}); err != nil {
			t.Fatalf("search with chat:session should succeed, got: %v", err)
		}
	})
}

// TestChatSearchEmptyTextRejected proves an empty query is refused before the
// store sees it: InvalidArgument over gRPC, and the canonical cairnerr
// envelope over the gateway so HTTP clients get the documented error shape.
func TestChatSearchEmptyTextRejected(t *testing.T) {
	t.Parallel()

	fix := startChatServer(t, &scriptedChatDriver{turns: []runner.Result{finalTurn("unused")}})

	_, err := fix.chat.SearchMessages(t.Context(), &cairnosv1.SearchMessagesRequest{Text: "   "})
	wantGRPCCode(t, err, codes.InvalidArgument)

	code, body := httpGet(t, fix.transport, fix.httpBase+"/v1/chat/search")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d (%s), want 400", code, body)
	}

	var envelope struct {
		Status string `json:"status"`
		Error  struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("parse error envelope %q: %v", body, err)
	}

	if envelope.Status != statusError || envelope.Error.Code != wireInvalidArgument {
		t.Fatalf("envelope = %s, want status error with code api.invalid_argument", body)
	}
}
