package chat_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/reliability"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/secrets"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// errUnknownWorkflow is the test trigger's complaint for an unexpected name.
var errUnknownWorkflow = errors.New("test trigger only knows the hello workflow")

// helloTOML is the mock workflow a confirmed trigger fires, kept cheap so the
// real engine runs it without an external service.
const helloTOML = `
[workflow]
name = "hello"

[action.greet]
runner = "mock"
params = { output = "hello from the workflow" }
`

// newAgent wires a chat agent over a real sqlite store, the scripted driver, a
// memory tool (seeded with one note), and an engine-backed workflow trigger, so
// a confirmed proposal fires a real run.
func newAgent(t *testing.T, driver *scriptedDriver, budget float64) (*chat.Agent, *sqlite.Store, *engineTrigger) {
	t.Helper()

	db := openStore(t)
	mem := openMemory(t)

	registry := newTriggerRegistry(t)
	eng := engine.New(engineConfig(), registry, db)
	trigger := &engineTrigger{eng: eng, db: db}

	tools := chat.BuildToolSet(map[string]runner.Runner{
		chat.ToolMemory: builtins.NewMemoryTool(mem),
	})

	agent := chat.New(&chat.Config{
		Store:         db,
		Driver:        driver,
		Tools:         tools.Tools,
		Specs:         tools.Specs,
		Personalities: chat.NewPersonalities(nil, ""),
		Trigger:       trigger,
		TurnBudgetUSD: budget,
		NewID:         sqlite.NewID,
	})

	return agent, db, trigger
}

// openMemory opens a memory store and seeds one entry so a memory tool call
// returns content the next prompt can show.
func openMemory(t *testing.T) *memory.SQLite {
	t.Helper()

	mem, err := memory.OpenSQLite(t.TempDir() + "/mem.db")
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}

	t.Cleanup(func() { _ = mem.Close() })

	if _, err := mem.Add(t.Context(), &memory.Entry{Scope: memScope, Content: memNote, Importance: 50}); err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	return mem
}

// newTriggerRegistry registers the mock runner the hello workflow uses.
func newTriggerRegistry(t *testing.T) *runner.Registry {
	t.Helper()

	registry := runner.NewRegistry()

	if err := registry.Register(builtins.MockName, builtins.MockFactory()); err != nil {
		t.Fatalf("register mock: %v", err)
	}

	if err := registry.BuildAll(nil); err != nil {
		t.Fatalf("build runners: %v", err)
	}

	return registry
}

// engineConfig is the minimal engine config for the trigger: a real DAG
// executor over the mock runner, no telemetry.
func engineConfig() engine.Config {
	return engine.Config{
		MaxConcurrentActions: 2,
		DefaultRetry:         reliability.Policy{MaxAttempts: 1},
		Secrets:              secrets.NewEnv(),
		NewID:                sqlite.NewID,
	}
}

// engineTrigger satisfies chat.WorkflowTrigger by running a parsed workflow
// through a real engine and reporting its terminal status. It proves the
// confirmed path fires a real run under engine governance, not a stub.
type engineTrigger struct {
	eng *engine.Engine
	db  *sqlite.Store

	mu       sync.Mutex
	runCalls int
}

// Run implements chat.WorkflowTrigger.
func (e *engineTrigger) Run(ctx context.Context, name string, inputs map[string]any) (string, <-chan chat.RunResult, error) {
	e.mu.Lock()
	e.runCalls++
	e.mu.Unlock()

	flow, err := workflow.Parse([]byte(helloTOML))
	if err != nil {
		return "", nil, err
	}

	if name != flow.Name {
		return "", nil, errUnknownWorkflow
	}

	run, err := e.eng.RunWorkflow(ctx, flow, inputs, "chat")
	if err != nil {
		return "", nil, err
	}

	// The fake sends the run's one result before returning the channel, so the
	// channel needs a slot to hold it.
	const resultBufferSize = 1

	done := make(chan chat.RunResult, resultBufferSize)
	done <- chat.RunResult{Status: string(run.Status)}

	return run.ID, done, nil
}

// runs reports how many times the trigger fired, so a test proves a denied
// proposal never reached the engine.
func (e *engineTrigger) runs() int {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.runCalls
}

// driveEvents is the channel-buffer size for one streamed call.
const driveEvents = 16

// drive runs one streaming call (SendMessage or ConfirmAction) to completion,
// collecting its events while the call streams, and returns the events with the
// call's error. The collector runs on its own goroutine because the call blocks
// until the turn finishes sending.
func drive(t *testing.T, call func(out chan<- chat.Event) error) ([]chat.Event, error) {
	t.Helper()

	events := make(chan chat.Event, driveEvents)

	var (
		collected []chat.Event
		group     sync.WaitGroup
	)

	group.Go(func() {
		collected = collectEvents(events)
	})

	callErr := call(events)

	group.Wait()

	if callErr != nil {
		return collected, fmt.Errorf("chat call: %w", callErr)
	}

	return collected, nil
}

// sendMessage runs one turn to completion and returns the streamed events plus
// the turn error.
func sendMessage(t *testing.T, agent *chat.Agent, sessionID, message string) ([]chat.Event, error) {
	t.Helper()

	return drive(t, func(out chan<- chat.Event) error {
		return agent.SendMessage(t.Context(), sessionID, message, out)
	})
}

// confirm runs one ConfirmAction to completion and returns the streamed events
// plus the error.
func confirm(t *testing.T, agent *chat.Agent, sessionID string, approve bool) ([]chat.Event, error) {
	t.Helper()

	return drive(t, func(out chan<- chat.Event) error {
		return agent.ConfirmAction(t.Context(), sessionID, approve, out)
	})
}

// newSession opens a session through the agent, failing the test on error.
func newSession(t *testing.T, agent *chat.Agent) string {
	t.Helper()

	session, err := agent.CreateSession(t.Context(), "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	return session.ID
}

// TestPlainQuestionAnswersWithoutTools proves the simplest turn: the driver
// answers directly, no tool runs, and the assistant reply persists.
func TestPlainQuestionAnswersWithoutTools(t *testing.T) {
	t.Parallel()

	driver := &scriptedDriver{turns: []runner.Result{finalTurn(plainAnswer)}}
	agent, db, _ := newAgent(t, driver, 0)
	sessionID := newSession(t, agent)

	events, err := sendMessage(t, agent, sessionID, "how is the budget")
	if err != nil {
		t.Fatalf("send message: %v", err)
	}

	if got := eventTypes(events); !reflect.DeepEqual(got, []chat.EventType{chat.EventAssistantChunk, chat.EventTurnDone}) {
		t.Fatalf("event types = %v, want assistant_chunk then turn_done", got)
	}

	if driver.callCount() != 1 {
		t.Errorf("driver called %d times, want 1: a plain answer needs one turn", driver.callCount())
	}

	assertLastAssistant(t, db, sessionID, plainAnswer)
}

// TestToolCallThenAnswer proves a memory tool call runs inline, its result
// feeds the next turn, and the agent then answers. The tool exchange persists
// as a tool-role message.
func TestToolCallThenAnswer(t *testing.T) {
	t.Parallel()

	driver := &scriptedDriver{turns: []runner.Result{
		toolTurn(chat.ToolMemory, map[string]any{argMemoryOp: memoryOpSearch, argMemoryQuery: "deploys"}),
		finalTurn(plainAnswer),
	}}
	agent, db, _ := newAgent(t, driver, 0)
	sessionID := newSession(t, agent)

	events, err := sendMessage(t, agent, sessionID, "what do you know about deploys")
	if err != nil {
		t.Fatalf("send message: %v", err)
	}

	wantTypes := []chat.EventType{chat.EventToolCall, chat.EventAssistantChunk, chat.EventTurnDone}
	if got := eventTypes(events); !reflect.DeepEqual(got, wantTypes) {
		t.Fatalf("event types = %v, want %v", got, wantTypes)
	}

	if driver.callCount() != 2 {
		t.Errorf("driver called %d times, want 2: one tool turn then one answer", driver.callCount())
	}

	// The second prompt must show the tool exchange, proving the result fed
	// back into the conversation.
	if prompt := driver.lastPrompt(t); !containsAll(prompt, chat.ToolMemory, memNote) {
		t.Errorf("second prompt missing the tool result; got:\n%s", prompt)
	}

	assertMessageRoles(t, db, sessionID, []string{store.ChatRoleUser, store.ChatRoleTool, store.ChatRoleAssistant})
}

// TestRunWorkflowProposalGatesUntilApproved is the confirmation gate's core: a run_workflow call
// raises a confirmation gate, no run starts, and only ConfirmAction(approve)
// fires the real workflow.
func TestRunWorkflowProposalGatesUntilApproved(t *testing.T) {
	t.Parallel()

	driver := &scriptedDriver{turns: []runner.Result{
		runWorkflowTurn(helloWorkflow, map[string]any{}),
	}}
	agent, _, trigger := newAgent(t, driver, 0)
	sessionID := newSession(t, agent)

	events, err := sendMessage(t, agent, sessionID, "run the hello workflow")
	if err != nil {
		t.Fatalf("send message: %v", err)
	}

	wantTypes := []chat.EventType{chat.EventConfirmationRequired, chat.EventTurnDone}
	if got := eventTypes(events); !reflect.DeepEqual(got, wantTypes) {
		t.Fatalf("event types = %v, want a confirmation gate then turn_done", got)
	}

	proposal := proposalFromEvents(t, events)
	if proposal.WorkflowName != helloWorkflow {
		t.Errorf("proposed workflow = %q, want %q", proposal.WorkflowName, helloWorkflow)
	}

	if trigger.runs() != 0 {
		t.Fatalf("trigger fired %d runs before confirmation, want 0: the gate must hold", trigger.runs())
	}

	confirmEvents, err := confirm(t, agent, sessionID, true)
	if err != nil {
		t.Fatalf("confirm approve: %v", err)
	}

	wantConfirm := []chat.EventType{chat.EventRunStarted, chat.EventRunFinished, chat.EventTurnDone}
	if got := eventTypes(confirmEvents); !reflect.DeepEqual(got, wantConfirm) {
		t.Fatalf("confirm event types = %v, want run_started, run_finished, turn_done", got)
	}

	if trigger.runs() != 1 {
		t.Errorf("trigger fired %d runs after approval, want 1", trigger.runs())
	}

	if status := runFinishedStatus(t, confirmEvents); status != string(store.RunSucceeded) {
		t.Errorf("run finished as %q, want succeeded", status)
	}
}

// TestRunWorkflowDenyNeverFires proves the deny path: ConfirmAction(false)
// clears the gate and the workflow never runs.
func TestRunWorkflowDenyNeverFires(t *testing.T) {
	t.Parallel()

	driver := &scriptedDriver{turns: []runner.Result{
		runWorkflowTurn(helloWorkflow, map[string]any{}),
	}}
	agent, _, trigger := newAgent(t, driver, 0)
	sessionID := newSession(t, agent)

	if _, err := sendMessage(t, agent, sessionID, "run the hello workflow"); err != nil {
		t.Fatalf("send message: %v", err)
	}

	denyEvents, err := confirm(t, agent, sessionID, false)
	if err != nil {
		t.Fatalf("confirm deny: %v", err)
	}

	if got := eventTypes(denyEvents); !reflect.DeepEqual(got, []chat.EventType{chat.EventTurnDone}) {
		t.Fatalf("deny event types = %v, want a single turn_done", got)
	}

	if trigger.runs() != 0 {
		t.Errorf("trigger fired %d runs on deny, want 0", trigger.runs())
	}

	// A second confirm has nothing pending: the gate cleared.
	if _, err := confirm(t, agent, sessionID, true); !hasCode(err, chat.CodeNoPendingAction) {
		t.Errorf("second confirm error = %v, want chat.no_pending_action", err)
	}
}

// TestTurnBudgetCapEndsTurn proves the per-turn budget wall: a driver that
// keeps calling a tool without answering burns its budget and the turn fails
// with the budget code.
func TestTurnBudgetCapEndsTurn(t *testing.T) {
	t.Parallel()

	// Every turn calls the memory tool and never answers; each turn costs
	// turnCost, so a budget just above one turn fails on the second iteration.
	loopTurn := toolTurn(chat.ToolMemory, map[string]any{argMemoryOp: memoryOpSearch, argMemoryQuery: "x"})
	driver := &scriptedDriver{turns: []runner.Result{loopTurn, loopTurn, loopTurn, loopTurn}}
	agent, _, _ := newAgent(t, driver, turnCost*1.5)
	sessionID := newSession(t, agent)

	_, err := sendMessage(t, agent, sessionID, "loop forever")
	if !hasCode(err, chat.CodeBudgetExhausted) {
		t.Fatalf("turn error = %v, want chat.budget_exhausted", err)
	}
}

// TestHistoryPersistsAndReplays proves conversation context carries across
// turns: a second turn's prompt contains the first turn's user and assistant
// messages.
func TestHistoryPersistsAndReplays(t *testing.T) {
	t.Parallel()

	const firstAnswer = "first answer"

	driver := &scriptedDriver{turns: []runner.Result{finalTurn(firstAnswer), finalTurn(plainAnswer)}}
	agent, db, _ := newAgent(t, driver, 0)
	sessionID := newSession(t, agent)

	if _, err := sendMessage(t, agent, sessionID, "first question"); err != nil {
		t.Fatalf("first turn: %v", err)
	}

	if _, err := sendMessage(t, agent, sessionID, "second question"); err != nil {
		t.Fatalf("second turn: %v", err)
	}

	if prompt := driver.lastPrompt(t); !containsAll(prompt, "first question", firstAnswer, "second question") {
		t.Errorf("second prompt did not replay the first turn; got:\n%s", prompt)
	}

	messages, err := agent.ListMessages(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}

	if len(messages) != 4 {
		t.Errorf("persisted %d messages, want 4 (two user, two assistant)", len(messages))
	}

	_ = db
}

// TestUnknownPersonalityRejected proves a typo'd personality fails at session
// creation rather than silently running the default.
func TestUnknownPersonalityRejected(t *testing.T) {
	t.Parallel()

	driver := &scriptedDriver{turns: []runner.Result{finalTurn(plainAnswer)}}
	agent, _, _ := newAgent(t, driver, 0)

	if _, err := agent.CreateSession(t.Context(), "nonexistent"); !errors.Is(err, chat.ErrUnknownPersonality) {
		t.Fatalf("create session error = %v, want ErrUnknownPersonality", err)
	}
}

// TestTurnDoneCarriesUsage proves the turn's accumulated token spend lands on
// the turn-done event, the field the OpenAI-compatible adapter reports as its
// usage block.
func TestTurnDoneCarriesUsage(t *testing.T) {
	t.Parallel()

	answered := runner.Result{
		Output: plainAnswer,
		Usage:  runner.Usage{InputTokens: 812, OutputTokens: 96, CostUSD: 0.02},
		Meta:   map[string]string{runner.MetaStopReason: runner.StopReasonEndTurn},
	}
	driver := &scriptedDriver{turns: []runner.Result{answered}}
	agent, _, _ := newAgent(t, driver, 0)
	sessionID := newSession(t, agent)

	events, err := sendMessage(t, agent, sessionID, "how is the budget")
	if err != nil {
		t.Fatalf("send message: %v", err)
	}

	done := turnDoneEvent(t, events)
	if done.Usage.InputTokens != 812 || done.Usage.OutputTokens != 96 {
		t.Errorf("turn-done usage = %+v, want input 812 output 96", done.Usage)
	}
}

// TestPersonalityNamesSortedNoDuplicates proves Names() lists the built-ins plus
// any config-added personalities in sorted order, and that a config entry that
// overrides a built-in name appears once, not twice (the overlay shares the map
// key). GET /v1/models renders this list.
func TestPersonalityNamesSortedNoDuplicates(t *testing.T) {
	t.Parallel()

	const configuredPersonality = "support"

	tests := []struct {
		name       string
		configured map[string]string
		want       []string
	}{
		{
			name:       "builtins_only",
			configured: nil,
			want:       []string{chat.PersonalityAssistant, chat.PersonalityOps},
		},
		{
			name:       "config_adds_name",
			configured: map[string]string{configuredPersonality: "be helpful"},
			want:       []string{chat.PersonalityAssistant, chat.PersonalityOps, configuredPersonality},
		},
		{
			name:       "override_builtin_no_duplicate",
			configured: map[string]string{chat.PersonalityAssistant: "reshaped assistant"},
			want:       []string{chat.PersonalityAssistant, chat.PersonalityOps},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := chat.NewPersonalities(tt.configured, "").Names()
			if !slices.Equal(got, tt.want) {
				t.Errorf("Names() = %v, want %v", got, tt.want)
			}
		})
	}
}

// turnDoneEvent pulls the terminating turn-done event out of a stream.
func turnDoneEvent(t *testing.T, events []chat.Event) chat.Event {
	t.Helper()

	for idx := range events {
		if events[idx].Type == chat.EventTurnDone {
			return events[idx]
		}
	}

	t.Fatal("no turn-done event in the stream")

	return chat.Event{}
}

// assertLastAssistant checks the most recent assistant message content.
func assertLastAssistant(t *testing.T, db *sqlite.Store, sessionID, want string) {
	t.Helper()

	messages, err := db.ListMessages(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}

	for _, message := range slices.Backward(messages) {
		if message.Role == store.ChatRoleAssistant {
			if message.Content != want {
				t.Errorf("last assistant message = %q, want %q", message.Content, want)
			}

			return
		}
	}

	t.Fatal("no assistant message persisted")
}

// assertMessageRoles checks the persisted message roles in order.
func assertMessageRoles(t *testing.T, db *sqlite.Store, sessionID string, want []string) {
	t.Helper()

	messages, err := db.ListMessages(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}

	roles := make([]string, len(messages))
	for idx, message := range messages {
		roles[idx] = message.Role
	}

	if !slices.Equal(roles, want) {
		t.Errorf("message roles = %v, want %v", roles, want)
	}
}

// proposalFromEvents pulls the proposal out of a confirmation-required event.
func proposalFromEvents(t *testing.T, events []chat.Event) *chat.Proposal {
	t.Helper()

	for idx := range events {
		if events[idx].Type == chat.EventConfirmationRequired {
			if events[idx].Proposal == nil {
				t.Fatal("confirmation event carried no proposal")
			}

			return events[idx].Proposal
		}
	}

	t.Fatal("no confirmation-required event in the stream")

	return nil
}

// runFinishedStatus pulls the terminal status out of a run-finished event.
func runFinishedStatus(t *testing.T, events []chat.Event) string {
	t.Helper()

	for idx := range events {
		if events[idx].Type == chat.EventRunFinished {
			return events[idx].RunStatus
		}
	}

	t.Fatal("no run-finished event in the stream")

	return ""
}

// hasCode reports whether err is a canonical error with the given code.
func hasCode(err error, code string) bool {
	cerr, ok := errors.AsType[*cairnerr.Error](err)

	return ok && cerr.Code == code
}

// containsAll reports whether text contains every substring.
func containsAll(text string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(text, sub) {
			return false
		}
	}

	return true
}
