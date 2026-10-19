package openaicompat_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/auth/authtest"
	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/nettest"
	"github.com/chadit/CairnOS/internal/openaicompat"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/pkg/runner"
)

// errGateDriverExecute guards the gate driver's plain Execute path; the chat
// turn only drives ExecuteWithTools.
var errGateDriverExecute = errors.New("gate driver only answers ExecuteWithTools")

// testRequestTimeout bounds a test request so a wedged handler fails the test
// instead of hanging it.
const testRequestTimeout = 10 * time.Second

// Shared fixture values; goconst applies to tests in this repo's lint policy.
const (
	completionsPath      = "/v1/chat/completions"
	personalityAssistant = "assistant"
	defaultSessionID     = "01JADAPTERSESSION0000000000"

	objectCompletion   = "chat.completion"
	completionIDPrefix = "chatcmpl-"
	finishStop         = "stop"

	plainAnswer           = "the budget is fine"
	workspaceAcme         = "acme"
	workflowDeployStaging = "deploy-staging"
	simpleUserBody        = `{"model":"assistant","messages":[{"role":"user","content":"hi"}]}`

	// The OpenAI error triple values the tests pin, plus the assistant role a
	// reply carries; each is spelled out so a production rename fails a test.
	codeUnauthenticated = "auth.unauthenticated"
	codeForbidden       = "auth.forbidden"
	codeModelNotFound   = "model_not_found"
	typeServerError     = "server_error"
	roleAssistant       = "assistant"

	// authTestSecret meets the 32-byte HS256 floor.
	authTestSecret = "0123456789abcdef0123456789abcdef"
	// hs256EnvName is the variable the authenticator reads its secret from.
	hs256EnvName = "CAIRNOS_AUTH_HS256_SECRET" //nolint:gosec // environment variable name, not a credential
)

// fakeAgent is an in-test ChatAgent that scripts one turn's events and records
// what CreateSession and SendMessage saw, so tests assert workspace stamping and
// the seeded prompt without a real driver.
type fakeAgent struct {
	createErr       error
	sendErr         error
	sessionID       string
	personality     string // resolved personality stamped on the returned session
	seenWorkspace   string
	seenPersonality string
	seenMessage     string
	events          []chat.Event
	sendCalls       int
	mu              sync.Mutex
}

// CreateSession records the workspace and personality it saw, then returns a
// session with the resolved personality or the scripted error.
func (f *fakeAgent) CreateSession(ctx context.Context, personality string) (*store.ChatSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.seenWorkspace = store.WorkspaceFromContext(ctx)
	f.seenPersonality = personality

	if f.createErr != nil {
		return nil, f.createErr
	}

	resolved := f.personality
	if personality != "" {
		resolved = personality
	}

	return &store.ChatSession{ID: f.sessionID, Personality: resolved, WorkspaceID: f.seenWorkspace}, nil
}

// SendMessage streams the scripted events and returns the scripted terminal
// error, closing out the way the real agent does.
func (f *fakeAgent) SendMessage(ctx context.Context, _, userMessage string, out chan<- chat.Event) error {
	defer close(out)

	f.mu.Lock()
	f.seenMessage = userMessage
	f.sendCalls++
	events := f.events
	sendErr := f.sendErr
	f.mu.Unlock()

	for idx := range events {
		select {
		case out <- events[idx]:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return sendErr
}

// workspace reports the workspace CreateSession saw, under the lock.
func (f *fakeAgent) workspace() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.seenWorkspace
}

// message reports the trailing user message SendMessage ran.
func (f *fakeAgent) message() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.seenMessage
}

// fakeStore records every seeded message so a test asserts the role sequence.
type fakeStore struct {
	appendErr error
	appended  []*store.ChatMessage
	mu        sync.Mutex
}

// AppendMessage records one message or returns the scripted error.
func (f *fakeStore) AppendMessage(_ context.Context, message *store.ChatMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.appendErr != nil {
		return f.appendErr
	}

	f.appended = append(f.appended, message)

	return nil
}

// roles returns the recorded message roles in order, under the lock.
func (f *fakeStore) roles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, len(f.appended))
	for idx := range f.appended {
		out[idx] = f.appended[idx].Role
	}

	return out
}

// count reports how many messages were seeded.
func (f *fakeStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.appended)
}

// fakeClock is an advanceable clock injected as the authenticator's Now, so the
// expiry test moves time past a token's exp without sleeping.
type fakeClock struct {
	now time.Time
	mu  sync.Mutex
}

// Now implements the auth.Config Now contract.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

// advance moves the clock forward.
func (c *fakeClock) advance(step time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(step)
}

// fixtureOptions tunes the service the fixture builds.
type fixtureOptions struct {
	agent        *fakeAgent
	maxBodyBytes int64
	authEnabled  bool
}

// fixture is one adapter service over fakes plus the httptest server tests drive
// it through.
type fixture struct {
	server    *nettest.Fake
	agent     *fakeAgent
	store     *fakeStore
	authStore *auth.SQLite
	authn     *auth.Authenticator
	clock     *fakeClock
}

// startFixture builds the adapter over the fakes, wiring a real authenticator
// when the options ask for enforcement, and mounts the Extension routes on a
// plain mux behind an httptest server, the same setup internal/mcp uses.
func startFixture(t *testing.T, opts fixtureOptions) *fixture {
	t.Helper()

	agent := opts.agent
	if agent == nil {
		agent = &fakeAgent{sessionID: defaultSessionID, personality: personalityAssistant}
	}

	memStore := &fakeStore{}

	compatCfg := &openaicompat.Config{
		Agent:         agent,
		Store:         memStore,
		Personalities: chat.NewPersonalities(nil, ""),
		MaxBodyBytes:  opts.maxBodyBytes,
		Logger:        slog.New(slog.DiscardHandler),
	}

	authStore, authn, clock := wireAuth(t, opts, compatCfg)

	service, err := openaicompat.NewService(compatCfg)
	if err != nil {
		t.Fatalf("build openai-compat service: %v", err)
	}

	memFix := mountService(t, service)
	memFix.agent = agent
	memFix.store = memStore
	memFix.authStore = authStore
	memFix.authn = authn
	memFix.clock = clock

	return memFix
}

// mountService mounts a service's Extension routes on a plain mux behind an
// in-process server, the shape internal/mcp uses and the real serve mux follows.
// It returns a fixture carrying only the server, for tests that build the
// service themselves.
func mountService(t *testing.T, service *openaicompat.Service) *fixture {
	t.Helper()

	mux := http.NewServeMux()
	for pattern, route := range service.Extension().HTTPRoutes {
		mux.HandleFunc(pattern, route)
	}

	server := nettest.New(mux)

	return &fixture{server: server}
}

// mountStreamed serves the same routes on an in-memory listener. The
// disconnect case reads a response while the handler is still writing it,
// which a recorder cannot do.
func mountStreamed(t *testing.T, service *openaicompat.Service) *nettest.Server {
	t.Helper()

	mux := http.NewServeMux()
	for pattern, route := range service.Extension().HTTPRoutes {
		mux.HandleFunc(pattern, route)
	}

	return nettest.NewServer(t, mux)
}

// probe issues a GET and returns the status code, so a test proves an unmounted
// path (the deliberately absent confirm route) answers 404.
func probe(t *testing.T, server *nettest.Fake, path string) int {
	t.Helper()

	url := server.URL + path

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("build probe request: %v", err)
	}

	client := server.Client()

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("probe %s: %v", url, err)
	}

	_ = resp.Body.Close()

	return resp.StatusCode
}

// getBody issues a GET and returns the response body, so a test tells which
// handler served a path apart.
func getBody(t *testing.T, server *nettest.Fake, path string) string {
	t.Helper()

	url := server.URL + path

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("build get request: %v", err)
	}

	client := server.Client()

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}

	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	return string(data)
}

// wireAuth opens an auth store and authenticator when the fixture asks for
// enforcement, injecting an advanceable clock so the expiry test controls time.
func wireAuth(t *testing.T, opts fixtureOptions, cfg *openaicompat.Config) (*auth.SQLite, *auth.Authenticator, *fakeClock) {
	t.Helper()

	if !opts.authEnabled {
		return nil, nil, nil
	}

	authStore, err := auth.OpenSQLite(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}

	t.Cleanup(func() { _ = authStore.Close() })

	material, err := auth.LoadMaterial(auth.AlgHS256, func(key string) string {
		if key == hs256EnvName {
			return authTestSecret
		}

		return ""
	})
	if err != nil {
		t.Fatalf("load material: %v", err)
	}

	clock := &fakeClock{now: time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)}

	authn := auth.New(authStore, material, auth.Config{
		HashCost:   authtest.FloorHashCost(),
		Issuer:     "cairnos-fixture",
		Audience:   "cairnos",
		TTL:        time.Hour,
		RequireExp: true,
		RequireJTI: true,
		Now:        clock.Now,
	})
	cfg.Auth = authn

	return authStore, authn, clock
}

// token seeds an enabled key with the given scopes in the default workspace and
// exchanges it for a bearer token.
func (f *fixture) token(t *testing.T, scopes ...string) string {
	t.Helper()

	return f.workspaceToken(t, "", scopes...)
}

// workspaceToken seeds an enabled key bound to workspace and exchanges it for a
// bearer token, so a test proves a session lands in the token's tenant.
func (f *fixture) workspaceToken(t *testing.T, workspace string, scopes ...string) string {
	t.Helper()

	key, plaintext, err := auth.NewKey("fixture", scopes, workspace, f.clock.Now(), authtest.FloorHashCost())
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}

	if innerErr := f.authStore.CreateKey(t.Context(), key); innerErr != nil {
		t.Fatalf("CreateKey: %v", innerErr)
	}

	tokenStr, _, err := f.authn.Login(t.Context(), plaintext)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	return tokenStr
}

// post drives a completions request with an optional bearer token, returning the
// status code and body. It returns the code rather than the response so the body
// is always closed here, keeping callers simple.
func (f *fixture) post(t *testing.T, token, body string) (int, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+completionsPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := f.server.Client()

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post completions: %v", err)
	}

	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	return resp.StatusCode, data
}

// errorBody is the decoded OpenAI error envelope a failing request returns.
type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// decodeError decodes the error envelope, failing the test on malformed JSON.
func decodeError(t *testing.T, data []byte) errorBody {
	t.Helper()

	var body errorBody
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decode error body %q: %v", data, err)
	}

	return body
}

// completionBody is the decoded chat.completion a successful request returns.
type completionBody struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
		Index        int    `json:"index"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

// decodeCompletion decodes a chat.completion, failing the test on malformed JSON.
func decodeCompletion(t *testing.T, data []byte) completionBody {
	t.Helper()

	var body completionBody
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decode completion body %q: %v", data, err)
	}

	return body
}

// chunkEvent scripts one assistant text chunk.
func chunkEvent(text string) chat.Event {
	return chat.Event{Type: chat.EventAssistantChunk, Text: text}
}

// doneEvent scripts the terminating turn-done event carrying usage.
func doneEvent(usage runner.Usage) chat.Event {
	return chat.Event{Type: chat.EventTurnDone, Usage: usage}
}

// confirmEvent scripts a D9 confirmation-required event.
func confirmEvent(proposal *chat.Proposal) chat.Event {
	return chat.Event{Type: chat.EventConfirmationRequired, Proposal: proposal}
}

// Shared D9-confirmation fixtures. The confirmation tests (non-streaming,
// streaming, and the real-agent path) assert the same proposal, so its inputs
// and the rendered substrings live in one place for goconst and DRY.
const (
	proposalInputsJSON = `{"env":"staging"}`
	proposalBudgetLine = "$0.50 USD"
	scopeWriteWorkflow = "write:workflow"
)

// stagingInputs is the deploy-staging run inputs the confirmation tests share.
func stagingInputs() map[string]any {
	return map[string]any{"env": "staging"}
}

// stagingProposal is the deploy-staging proposal the confirmation tests raise.
func stagingProposal() *chat.Proposal {
	return &chat.Proposal{
		WorkflowName:       workflowDeployStaging,
		Inputs:             stagingInputs(),
		EstimatedBudgetUSD: 0.5,
	}
}

// confirmationWants lists the substrings a rendered D9 confirmation must carry:
// the workflow, its inputs as compact JSON, the budget, the session id, and the
// native write:workflow scope a human needs to approve.
func confirmationWants(sessionID string) []string {
	return []string{workflowDeployStaging, proposalInputsJSON, proposalBudgetLine, sessionID, scopeWriteWorkflow}
}

// answerAgent builds a fake agent that answers with one plain chunk, the stand-in
// for a turn the auth and wiring cases do not otherwise script.
func answerAgent() *fakeAgent {
	return &fakeAgent{
		sessionID:   defaultSessionID,
		personality: personalityAssistant,
		events:      []chat.Event{chunkEvent(plainAnswer), doneEvent(runner.Usage{})},
	}
}

// authCase is one row of the chat:session gate: the token a request carries and
// the status and error code it must earn.
type authCase struct {
	token      func(t *testing.T, fix *fixture) string
	name       string
	wantCode   string
	wantStatus int
}

// runAuthCases drives the chat:session gate table through one route's send, so
// completions and models enforce the same rows and cannot drift apart: missing
// and invalid tokens 401, a wrong scope 403, and a chat:session token 200.
func runAuthCases(t *testing.T, send func(t *testing.T, fix *fixture, token string) (int, []byte)) {
	t.Helper()

	cases := []authCase{
		{
			name:       "missing_token",
			token:      func(_ *testing.T, _ *fixture) string { return "" },
			wantStatus: http.StatusUnauthorized,
			wantCode:   codeUnauthenticated,
		},
		{
			name:       "invalid_token",
			token:      func(_ *testing.T, _ *fixture) string { return "not-a-real-token" },
			wantStatus: http.StatusUnauthorized,
			wantCode:   codeUnauthenticated,
		},
		{
			name: "wrong_scope",
			token: func(t *testing.T, fix *fixture) string {
				t.Helper()

				return fix.token(t, auth.ScopeReadMemory)
			},
			wantStatus: http.StatusForbidden,
			wantCode:   codeForbidden,
		},
		{
			name: "chat_session_scope",
			token: func(t *testing.T, fix *fixture) string {
				t.Helper()

				return fix.token(t, auth.ScopeChatSession)
			},
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fix := startFixture(t, fixtureOptions{authEnabled: true, agent: answerAgent()})

			status, data := send(t, fix, tt.token(t, fix))
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", status, tt.wantStatus, data)
			}

			if tt.wantCode == "" {
				return
			}

			if got := decodeError(t, data).Error.Code; got != tt.wantCode {
				t.Errorf("code = %q, want %q", got, tt.wantCode)
			}
		})
	}
}
