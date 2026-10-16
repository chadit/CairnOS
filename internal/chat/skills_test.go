package chat_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/pkg/runner"
)

// skillHeader is the leading marker of an injected skill section, factored out
// so the inert-message assertions share one literal.
const skillHeader = "Skill:"

// shouldNotLoad is the body a mis-fired grammar would inject; its presence in a
// prompt would prove a negative case wrongly loaded.
const shouldNotLoad = "should not load"

// fakeSkillSource is an in-test chat.SkillSource over a name->body map, so the
// chat turn's slash path runs without the real skills library on disk.
type fakeSkillSource struct {
	bodies map[string]string
	infos  []chat.SkillInfo
}

var _ chat.SkillSource = (*fakeSkillSource)(nil)

// List implements chat.SkillSource.
func (f *fakeSkillSource) List() []chat.SkillInfo { return f.infos }

// Load implements chat.SkillSource, returning ErrSkillNotFound for an unknown
// name the way the real adapter does.
func (f *fakeSkillSource) Load(name string) (string, error) {
	body, ok := f.bodies[name]
	if !ok {
		return "", chat.ErrSkillNotFound
	}

	return body, nil
}

// newSkillAgent wires a lean chat agent over a real sqlite store, the scripted
// driver, and the given skill source (nil disables skills). No tools or trigger:
// these turns exercise slash injection, not tool calls.
func newSkillAgent(t *testing.T, driver *scriptedDriver, source chat.SkillSource) *chat.Agent {
	t.Helper()

	db := openStore(t)

	return chat.New(&chat.Config{
		Store:         db,
		Driver:        driver,
		Personalities: chat.NewPersonalities(nil, ""),
		Skills:        source,
		NewID:         sqlite.NewID,
	})
}

// hasToolNote reports whether a tool-role message with exactly this content was
// persisted.
func hasToolNote(messages []*store.ChatMessage, want string) bool {
	for _, msg := range messages {
		if msg.Role == store.ChatRoleTool && msg.Content == want {
			return true
		}
	}

	return false
}

// hasAnyToolNote reports whether any tool-role message was persisted, so an
// inert case can prove none was.
func hasAnyToolNote(messages []*store.ChatMessage) bool {
	for _, msg := range messages {
		if msg.Role == store.ChatRoleTool {
			return true
		}
	}

	return false
}

// TestSlashSkillInjectsBody proves the end-to-end slash path: /name injects the
// skill body as a leading Skill section, the persisted user message keeps the
// raw slash text, and a tool-role note records the load.
func TestSlashSkillInjectsBody(t *testing.T) {
	t.Parallel()

	const body = "Summarize the conversation into five bullets."

	source := &fakeSkillSource{bodies: map[string]string{skillSummarize: body}}
	driver := &scriptedDriver{turns: []runner.Result{finalTurn(plainAnswer)}}
	agent := newSkillAgent(t, driver, source)
	sessionID := newSession(t, agent)

	const message = "/summarize the deploy thread"

	if _, err := sendMessage(t, agent, sessionID, message); err != nil {
		t.Fatalf("send message: %v", err)
	}

	if prompt := driver.lastPrompt(t); !containsAll(prompt, "Skill: summarize", body) {
		t.Errorf("prompt missing the skill section; got:\n%s", prompt)
	}

	messages, err := agent.ListMessages(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}

	if messages[0].Role != store.ChatRoleUser || messages[0].Content != message {
		t.Errorf("first message = {%s, %q}, want the raw user slash text", messages[0].Role, messages[0].Content)
	}

	if !hasToolNote(messages, `loaded skill "summarize" for this turn`) {
		t.Errorf("no loaded-skill tool note persisted; messages = %+v", messages)
	}
}

// TestSlashUnknownSkillNotesAndProceeds proves an unknown but grammar-valid name
// records a visible note, injects nothing, and still answers normally.
func TestSlashUnknownSkillNotesAndProceeds(t *testing.T) {
	t.Parallel()

	source := &fakeSkillSource{bodies: map[string]string{}}
	driver := &scriptedDriver{turns: []runner.Result{finalTurn(plainAnswer)}}
	agent := newSkillAgent(t, driver, source)
	sessionID := newSession(t, agent)

	events, err := sendMessage(t, agent, sessionID, "/nope do the thing")
	if err != nil {
		t.Fatalf("send message: %v", err)
	}

	wantTypes := []chat.EventType{chat.EventAssistantChunk, chat.EventTurnDone}
	if got := eventTypes(events); !reflect.DeepEqual(got, wantTypes) {
		t.Fatalf("event types = %v, want a normal answered turn", got)
	}

	if strings.Contains(driver.lastPrompt(t), skillHeader) {
		t.Errorf("unknown skill still injected a skill section")
	}

	messages, err := agent.ListMessages(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}

	if !hasToolNote(messages, `no skill named "nope" is available`) {
		t.Errorf("no unavailable-skill note persisted; messages = %+v", messages)
	}
}

// TestSlashGrammarNegativesInert pins the first-token grammar: each negative
// leaves the token inert, so no skill section is injected and no tool note is
// persisted, even when the source would happily load the mis-parsed name.
func TestSlashGrammarNegativesInert(t *testing.T) {
	t.Parallel()

	negatives := []string{
		"/usr/local/bin fails",
		"see /etc/hosts",
		"/Deploy the box",
		"//x is odd",
		"/ just a slash",
	}

	for _, message := range negatives {
		t.Run(message, func(t *testing.T) {
			t.Parallel()

			source := &fakeSkillSource{bodies: map[string]string{
				"usr": shouldNotLoad, "x": shouldNotLoad, "deploy": shouldNotLoad,
			}}
			driver := &scriptedDriver{turns: []runner.Result{finalTurn(plainAnswer)}}
			agent := newSkillAgent(t, driver, source)
			sessionID := newSession(t, agent)

			if _, err := sendMessage(t, agent, sessionID, message); err != nil {
				t.Fatalf("send message: %v", err)
			}

			if prompt := driver.lastPrompt(t); strings.Contains(prompt, skillHeader) {
				t.Errorf("inert message %q injected a skill section:\n%s", message, prompt)
			}

			messages, err := agent.ListMessages(t.Context(), sessionID)
			if err != nil {
				t.Fatalf("list messages: %v", err)
			}

			if hasAnyToolNote(messages) {
				t.Errorf("inert message %q produced a tool note", message)
			}
		})
	}
}

// TestNilSkillSourceInert proves a nil Skills source leaves a slash token inert:
// no injection, no note, a normal turn.
func TestNilSkillSourceInert(t *testing.T) {
	t.Parallel()

	driver := &scriptedDriver{turns: []runner.Result{finalTurn(plainAnswer)}}
	agent := newSkillAgent(t, driver, nil)
	sessionID := newSession(t, agent)

	if _, err := sendMessage(t, agent, sessionID, "/summarize now"); err != nil {
		t.Fatalf("send message: %v", err)
	}

	if strings.Contains(driver.lastPrompt(t), skillHeader) {
		t.Errorf("nil source still injected a skill section")
	}

	messages, err := agent.ListMessages(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}

	if hasAnyToolNote(messages) {
		t.Errorf("nil source produced a tool note")
	}
}
