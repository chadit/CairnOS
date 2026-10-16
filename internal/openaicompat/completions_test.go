package openaicompat_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/openaicompat"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/pkg/runner"
)

// TestNonStreamingPlainAnswer proves a plain turn returns the OpenAI
// chat.completion shape: the agent's answer as the choice content, real usage
// counts, an id embedding the session, and the resolved personality as model.
func TestNonStreamingPlainAnswer(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{
		sessionID:   defaultSessionID,
		personality: personalityAssistant,
		events: []chat.Event{
			chunkEvent(plainAnswer),
			doneEvent(runner.Usage{InputTokens: 812, OutputTokens: 96}),
		},
	}
	fix := startFixture(t, fixtureOptions{agent: agent})

	status, data := fix.post(t, "", `{"model":"assistant","messages":[{"role":"user","content":"how is the budget"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, data)
	}

	body := decodeCompletion(t, data)

	if body.Object != objectCompletion {
		t.Errorf("object = %q, want %q", body.Object, objectCompletion)
	}

	if body.ID != completionIDPrefix+defaultSessionID {
		t.Errorf("id = %q, want %s%s", body.ID, completionIDPrefix, defaultSessionID)
	}

	if body.Model != personalityAssistant {
		t.Errorf("model = %q, want %q", body.Model, personalityAssistant)
	}

	if len(body.Choices) != 1 || body.Choices[0].Message.Content != plainAnswer {
		t.Fatalf("choices = %+v, want one assistant answer %q", body.Choices, plainAnswer)
	}

	if body.Choices[0].Message.Role != roleAssistant || body.Choices[0].FinishReason != finishStop {
		t.Errorf("choice = %+v, want assistant role and stop finish", body.Choices[0])
	}

	if body.Usage.PromptTokens != 812 || body.Usage.CompletionTokens != 96 || body.Usage.TotalTokens != 908 {
		t.Errorf("usage = %+v, want 812/96/908", body.Usage)
	}

	if got := fix.agent.message(); got != "how is the budget" {
		t.Errorf("agent ran message %q, want the trailing user text", got)
	}
}

// TestMultiChunkAnswerConcatenates proves several assistant chunks join into one
// content string.
func TestMultiChunkAnswerConcatenates(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{
		sessionID:   defaultSessionID,
		personality: personalityAssistant,
		events: []chat.Event{
			chunkEvent("Hello, "),
			chunkEvent("world"),
			doneEvent(runner.Usage{}),
		},
	}
	fix := startFixture(t, fixtureOptions{agent: agent})

	status, data := fix.post(t, "", simpleUserBody)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, data)
	}

	if got := decodeCompletion(t, data).Choices[0].Message.Content; got != "Hello, world" {
		t.Errorf("content = %q, want the joined chunks", got)
	}
}

// TestConfirmationRendersAsAssistantText proves a D9 gate surfaces as assistant
// text naming the pending proposal, and that this surface exposes no confirm
// route, so a foreign client can never fire the workflow.
func TestConfirmationRendersAsAssistantText(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{
		sessionID:   defaultSessionID,
		personality: personalityAssistant,
		events:      []chat.Event{confirmEvent(stagingProposal()), doneEvent(runner.Usage{})},
	}
	fix := startFixture(t, fixtureOptions{agent: agent})

	status, data := fix.post(t, "", simpleUserBody)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, data)
	}

	content := decodeCompletion(t, data).Choices[0].Message.Content
	for _, want := range confirmationWants(defaultSessionID) {
		if !strings.Contains(content, want) {
			t.Errorf("confirmation text missing %q; got:\n%s", want, content)
		}
	}

	// The adapter mounts only the completions route, so a confirm path on this
	// surface is not routed: a foreign client has no way to approve.
	notFound := probe(t, fix.server, "/v1/chat/sessions/"+defaultSessionID+"/confirm")
	if notFound != http.StatusNotFound {
		t.Errorf("confirm route status = %d, want 404 (no confirm route by design)", notFound)
	}
}

// TestTurnErrorReturnsServerError proves a terminal turn failure renders as 500
// server_error carrying the canonical chat.* code.
func TestTurnErrorReturnsServerError(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{
		sessionID:   defaultSessionID,
		personality: personalityAssistant,
		sendErr:     cairnerr.New(chat.CodeDriverFailed, "chat driver failed"),
	}
	fix := startFixture(t, fixtureOptions{agent: agent})

	status, data := fix.post(t, "", simpleUserBody)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body: %s)", status, data)
	}

	body := decodeError(t, data)
	if body.Error.Type != typeServerError || body.Error.Code != chat.CodeDriverFailed {
		t.Errorf("error = %+v, want server_error / %s", body.Error, chat.CodeDriverFailed)
	}
}

// TestAuthorization proves the chat:session gate: a missing or bad token is 401,
// a token without the scope is 403, and a scoped token answers 200, all in the
// OpenAI error shape.
func TestAuthorization(t *testing.T) {
	t.Parallel()

	runAuthCases(t, func(t *testing.T, fix *fixture, token string) (int, []byte) {
		t.Helper()

		return fix.post(t, token, simpleUserBody)
	})
}

// TestExpiredTokenUnauthorized proves an expired token gets its own 401 code so
// a client knows to re-login.
func TestExpiredTokenUnauthorized(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{authEnabled: true, agent: answerAgent()})
	token := fix.token(t, auth.ScopeChatSession)

	fix.clock.advance(2 * time.Hour) // past the one-hour TTL

	status, data := fix.post(t, token, simpleUserBody)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body: %s)", status, data)
	}

	if got := decodeError(t, data).Error.Code; got != "auth.token_expired" {
		t.Errorf("code = %q, want auth.token_expired", got)
	}
}

// TestWorkspaceStampedFromToken proves the session lands in the token's tenant,
// the same stamping the gRPC interceptor gives ChatService.
func TestWorkspaceStampedFromToken(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{authEnabled: true, agent: answerAgent()})
	token := fix.workspaceToken(t, workspaceAcme, auth.ScopeChatSession)

	status, data := fix.post(t, token, simpleUserBody)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, data)
	}

	if got := fix.agent.workspace(); got != workspaceAcme {
		t.Errorf("session workspace = %q, want %q", got, workspaceAcme)
	}
}

// TestAuthDisabledUsesDefaultWorkspace proves a tokenless call runs on the
// default workspace, matching the MCP posture.
func TestAuthDisabledUsesDefaultWorkspace(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{agent: answerAgent()})

	status, data := fix.post(t, "", simpleUserBody)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, data)
	}

	if got := fix.agent.workspace(); got != store.DefaultWorkspace {
		t.Errorf("session workspace = %q, want default", got)
	}
}

// TestSeedsPriorHistoryInOrder proves every message but the trailing one seeds
// into the fresh session in order, with the system role mapped through, and the
// trailing user message runs as the turn.
func TestSeedsPriorHistoryInOrder(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{agent: answerAgent()})

	body := `{"model":"assistant","messages":[` +
		`{"role":"system","content":"be terse"},` +
		`{"role":"user","content":"first"},` +
		`{"role":"assistant","content":"prior reply"},` +
		`{"role":"user","content":"now this"}]}`

	status, data := fix.post(t, "", body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, data)
	}

	wantRoles := []string{store.ChatRoleSystem, store.ChatRoleUser, store.ChatRoleAssistant}
	if got := fix.store.roles(); !slices.Equal(got, wantRoles) {
		t.Errorf("seeded roles = %v, want %v", got, wantRoles)
	}

	if got := fix.agent.message(); got != "now this" {
		t.Errorf("turn ran message %q, want the trailing user text", got)
	}
}

// TestTextContentPartAccepted proves array content of text parts concatenates
// into the message text.
func TestTextContentPartAccepted(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{agent: answerAgent()})

	body := `{"model":"assistant","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"one "},{"type":"text","text":"two"}]}]}`

	status, data := fix.post(t, "", body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, data)
	}

	if got := fix.agent.message(); got != "one two" {
		t.Errorf("turn ran message %q, want the joined text parts", got)
	}
}

// TestValidationRejections table-drives the 400 request-shape failures plus the
// stream and model cases, each mapping to its canonical code.
func TestValidationRejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		wantCode   string
		wantStatus int
	}{
		{name: "empty_messages", body: `{"model":"assistant","messages":[]}`, wantStatus: http.StatusBadRequest, wantCode: "missing_messages"},
		{name: "trailing_assistant", body: `{"model":"assistant","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`, wantStatus: http.StatusBadRequest, wantCode: "trailing_message_not_user"},
		{name: "non_text_part", body: `{"model":"assistant","messages":[{"role":"user","content":[{"type":"image_url","image_url":"x"}]}]}`, wantStatus: http.StatusBadRequest, wantCode: "unsupported_content_part"},
		{name: "malformed_json", body: `{"model":`, wantStatus: http.StatusBadRequest, wantCode: "invalid_body"},
		{name: "unknown_model", body: `{"model":"banana","messages":[{"role":"user","content":"hi"}]}`, wantStatus: http.StatusNotFound, wantCode: codeModelNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fix := startFixture(t, fixtureOptions{agent: answerAgent()})

			status, data := fix.post(t, "", tt.body)
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", status, tt.wantStatus, data)
			}

			if got := decodeError(t, data).Error.Code; got != tt.wantCode {
				t.Errorf("code = %q, want %q", got, tt.wantCode)
			}

			if fix.store.count() != 0 {
				t.Errorf("seeded %d messages on a rejected request, want 0", fix.store.count())
			}
		})
	}
}

// TestCreateSessionUnknownPersonalityIs404 proves an unknown personality from
// CreateSession maps to 404 model_not_found even when the pre-check passes.
func TestCreateSessionUnknownPersonalityIs404(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{sessionID: defaultSessionID, createErr: chat.ErrUnknownPersonality}
	fix := startFixture(t, fixtureOptions{agent: agent})

	// "ops" is a built-in, so the pre-check passes and CreateSession is the one
	// that reports the unknown personality.
	status, data := fix.post(t, "", `{"model":"ops","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", status, data)
	}

	if got := decodeError(t, data).Error.Code; got != codeModelNotFound {
		t.Errorf("code = %q, want model_not_found", got)
	}
}

// TestBodyOverCapRejected proves an over-limit body answers 413
// request_too_large, since extension routes apply the cap themselves.
func TestBodyOverCapRejected(t *testing.T) {
	t.Parallel()

	fix := startFixture(t, fixtureOptions{agent: answerAgent(), maxBodyBytes: 32})

	status, data := fix.post(t, "", simpleUserBody)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body: %s)", status, data)
	}

	failure := decodeError(t, data).Error
	if failure.Code != "request_too_large" {
		t.Errorf("code = %q, want request_too_large", failure.Code)
	}

	if !strings.Contains(failure.Message, "32-byte") {
		t.Errorf("message = %q, want it to name the 32-byte limit", failure.Message)
	}
}

// TestRealAgentSeedsPromptAndGatesWorkflow drives a real chat.Agent over a
// scripted driver and the sqlite store: the driver prompt replays the seeded
// system/user/assistant lines, and a run_workflow call produces confirmation
// text naming the workflow, inputs, budget, and session, with no run started
// (the trigger is nil).
func TestRealAgentSeedsPromptAndGatesWorkflow(t *testing.T) {
	t.Parallel()

	db, err := sqlite.Open(t.TempDir() + "/chat.db")
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	driver := &gateDriver{workflow: workflowDeployStaging, inputs: stagingInputs()}
	tools := chat.BuildToolSet(map[string]runner.Runner{})

	const budget = 0.5

	agent := chat.New(&chat.Config{
		Store:         db,
		Driver:        driver,
		Tools:         tools.Tools,
		Specs:         tools.Specs,
		Personalities: chat.NewPersonalities(nil, ""),
		Trigger:       nil, // no trigger wired: a proposal can never fire a run here
		TurnBudgetUSD: budget,
		NewID:         store.NewID,
	})

	service, err := openaicompat.NewService(&openaicompat.Config{
		Agent:         agent,
		Store:         db,
		Personalities: chat.NewPersonalities(nil, ""),
	})
	if err != nil {
		t.Fatalf("build service: %v", err)
	}

	fix := mountService(t, service)

	body := `{"model":"assistant","messages":[` +
		`{"role":"system","content":"be a terse ops assistant"},` +
		`{"role":"user","content":"first question"},` +
		`{"role":"assistant","content":"prior answer"},` +
		`{"role":"user","content":"run the deploy"}]}`

	status, data := fix.post(t, "", body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, data)
	}

	prompt := driver.lastPrompt(t)
	for _, want := range []string{"system: be a terse ops assistant", "user: first question", "assistant: prior answer", "user: run the deploy"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("driver prompt missing %q; got:\n%s", want, prompt)
		}
	}

	content := decodeCompletion(t, data).Choices[0].Message.Content
	for _, want := range []string{workflowDeployStaging, proposalInputsJSON, proposalBudgetLine, scopeWriteWorkflow} {
		if !strings.Contains(content, want) {
			t.Errorf("confirmation text missing %q; got:\n%s", want, content)
		}
	}

	// The run store holds no run: the gate proposed a workflow but nothing fired.
	runs, err := db.ListRuns(t.Context(), store.ListFilter{})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}

	if len(runs) != 0 {
		t.Errorf("started %d runs through the gate, want 0", len(runs))
	}
}

// gateDriver is an in-test runner.ToolCaller that records the prompt it saw and
// answers with one run_workflow tool call, the D9 gate the chat agent
// intercepts.
type gateDriver struct {
	inputs   map[string]any
	workflow string
	prompts  []string
	mu       sync.Mutex
}

var _ runner.ToolCaller = (*gateDriver)(nil)

// Name implements runner.Runner.
func (*gateDriver) Name() string { return "gate-driver" }

// Validate implements runner.Runner.
func (*gateDriver) Validate(_ *runner.Task) error { return nil }

// Execute implements runner.Runner; the chat turn only uses ExecuteWithTools.
func (*gateDriver) Execute(_ context.Context, _ *runner.Task) (runner.Result, error) {
	return runner.Result{}, errGateDriverExecute
}

// ExecuteWithTools records the prompt and returns a run_workflow tool call.
func (d *gateDriver) ExecuteWithTools(_ context.Context, task *runner.Task, _ []runner.ToolSpec) (runner.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	prompt, _ := task.Params["prompt"].(string)
	d.prompts = append(d.prompts, prompt)

	return runner.Result{
		Output: map[string]any{
			runner.ToolCallName:      chat.RunWorkflowToolName,
			runner.ToolCallArguments: map[string]any{"workflow": d.workflow, "inputs": d.inputs},
		},
		Meta: map[string]string{runner.MetaStopReason: runner.StopReasonToolUse},
	}, nil
}

// lastPrompt returns the most recent prompt the driver saw.
func (d *gateDriver) lastPrompt(t *testing.T) string {
	t.Helper()

	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.prompts) == 0 {
		t.Fatal("driver saw no prompts")
	}

	return d.prompts[len(d.prompts)-1]
}
