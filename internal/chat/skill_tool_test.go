package chat_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Shared fixtures for the description-match tests. goconst applies to tests in
// this repo's lint policy, so the repeated skill name and markers are consts.
const (
	skillNameBig    = "big"
	truncatedMarker = "... (truncated)"
	skillListHeader = "Available skills (load one with the skill tool when its description matches the task):"
)

// stubDescriber is a minimal runner.Describer standing in for the memory and
// vault read tools in the spec-order test, so the assertion does not need real
// stores to prove the descriptor ordering.
type stubDescriber struct {
	name string
}

var _ runner.Describer = stubDescriber{}

func (s stubDescriber) Name() string              { return s.name }
func (stubDescriber) Validate(*runner.Task) error { return nil }
func (stubDescriber) Execute(context.Context, *runner.Task) (runner.Result, error) {
	return runner.Result{}, nil
}
func (s stubDescriber) Describe() runner.ToolSpec { return runner.ToolSpec{Name: s.name} }

// bigResultTool is a read tool returning a large string, so a test can prove the
// non-skill truncation path still clips at the tool-result cap.
type bigResultTool struct {
	output string
}

var _ runner.Runner = (*bigResultTool)(nil)

func (*bigResultTool) Name() string                { return "bigtool" }
func (*bigResultTool) Validate(*runner.Task) error { return nil }
func (b *bigResultTool) Execute(context.Context, *runner.Task) (runner.Result, error) {
	return runner.Result{Output: b.output}, nil
}

// newSkillToolAgent wires a chat agent whose read tools include the skill tool
// over the given source, so a scripted driver can call skill(name=...) and the
// persisted record can be inspected.
func newSkillToolAgent(t *testing.T, driver *scriptedDriver, source chat.SkillSource) *chat.Agent {
	t.Helper()

	db := openStore(t)
	tools := chat.BuildToolSet(map[string]runner.Runner{
		chat.SkillToolName: chat.NewSkillTool(source),
	})

	return chat.New(&chat.Config{
		Store:         db,
		Driver:        driver,
		Tools:         tools.Tools,
		Specs:         tools.Specs,
		Personalities: chat.NewPersonalities(nil, ""),
		Skills:        source,
		NewID:         sqlite.NewID,
	})
}

// onlyToolRecord returns the single tool-role message content for a session,
// failing when there is not exactly one.
func onlyToolRecord(t *testing.T, agent *chat.Agent, sessionID string) string {
	t.Helper()

	messages, err := agent.ListMessages(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}

	var records []string

	for _, message := range messages {
		if message.Role == store.ChatRoleTool {
			records = append(records, message.Content)
		}
	}

	if len(records) != 1 {
		t.Fatalf("want exactly one tool-role record, got %d: %v", len(records), records)
	}

	return records[0]
}

// head returns the first n bytes of s for bounded failure messages.
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n]
}

// TestSystemPromptListsSkills proves the description-match listing appends skill
// names and descriptions to the system prompt when the source lists any.
func TestSystemPromptListsSkills(t *testing.T) {
	t.Parallel()

	source := &fakeSkillSource{infos: []chat.SkillInfo{
		{Name: "deploy-check", Description: "Verify a host before deploy."},
		{Name: "summarize", Description: "Summarize the conversation into five bullets."},
	}}
	driver := &scriptedDriver{turns: []runner.Result{finalTurn(plainAnswer)}}
	agent := newSkillAgent(t, driver, source)
	sessionID := newSession(t, agent)

	if _, err := sendMessage(t, agent, sessionID, "how is the box"); err != nil {
		t.Fatalf("send message: %v", err)
	}

	system := driver.lastSystem(t)
	if !containsAll(
		system,
		skillListHeader,
		"- deploy-check: Verify a host before deploy.",
		"- summarize: Summarize the conversation into five bullets.",
	) {
		t.Errorf("system prompt missing the skills listing; got:\n%s", system)
	}
}

// TestSystemPromptOmitsSkills proves the listing is absent when the source lists
// nothing or is nil, so the section never shows with nothing to offer.
func TestSystemPromptOmitsSkills(t *testing.T) {
	t.Parallel()

	tests := []struct {
		source chat.SkillSource
		name   string
	}{
		{name: "empty_list", source: &fakeSkillSource{}},
		{name: "nil_source", source: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			driver := &scriptedDriver{turns: []runner.Result{finalTurn(plainAnswer)}}
			agent := newSkillAgent(t, driver, tt.source)
			sessionID := newSession(t, agent)

			if _, err := sendMessage(t, agent, sessionID, "hi"); err != nil {
				t.Fatalf("send message: %v", err)
			}

			if strings.Contains(driver.lastSystem(t), "Available skills") {
				t.Errorf("system prompt carried a skills listing with a %s source", tt.name)
			}
		})
	}
}

// TestSkillToolSpecsOrder proves BuildToolSet orders the descriptors memory,
// vault_query, skill, then run_workflow last.
func TestSkillToolSpecsOrder(t *testing.T) {
	t.Parallel()

	set := chat.BuildToolSet(map[string]runner.Runner{
		chat.ToolMemory:     stubDescriber{name: chat.ToolMemory},
		chat.ToolVaultQuery: stubDescriber{name: chat.ToolVaultQuery},
		chat.SkillToolName:  chat.NewSkillTool(&fakeSkillSource{}),
	})

	names := make([]string, len(set.Specs))
	for idx := range set.Specs {
		names[idx] = set.Specs[idx].Name
	}

	want := []string{chat.ToolMemory, chat.ToolVaultQuery, chat.SkillToolName, chat.RunWorkflowToolName}
	if !slices.Equal(names, want) {
		t.Errorf("spec order = %v, want %v", names, want)
	}
}

// TestSkillToolPersistsFullBody is the truncation-parity case: a body over the
// 4 KiB tool-result cap and under the 16 KiB load cap persists whole through the
// tool exchange, byte-identical to what a slash invocation would inject.
func TestSkillToolPersistsFullBody(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("A", 8000)
	source := &fakeSkillSource{bodies: map[string]string{skillNameBig: body}}

	driver := &scriptedDriver{turns: []runner.Result{
		toolTurn(chat.SkillToolName, map[string]any{"name": skillNameBig}),
		finalTurn(plainAnswer),
	}}
	agent := newSkillToolAgent(t, driver, source)
	sessionID := newSession(t, agent)

	if _, err := sendMessage(t, agent, sessionID, "load the big skill"); err != nil {
		t.Fatalf("send message: %v", err)
	}

	record := onlyToolRecord(t, agent, sessionID)
	if strings.Contains(record, truncatedMarker) {
		t.Fatalf("skill tool record truncated a 4-16 KiB body:\n%s", head(record, 200))
	}

	wantPrefix := fmt.Sprintf("loaded skill %q (%d bytes) body:\n", skillNameBig, len(body))
	if !strings.HasPrefix(record, wantPrefix) {
		t.Fatalf("record prefix = %q, want %q", head(record, len(wantPrefix)), wantPrefix)
	}

	toolBody := strings.TrimPrefix(record, wantPrefix)
	if toolBody != body {
		t.Errorf("tool-path body differs from the source body")
	}

	slashBody := slashInjectedBody(t, source)
	if toolBody != slashBody {
		t.Errorf("tool-path body is not byte-identical to the slash-path body")
	}
}

// slashInjectedBody runs the /big slash path over the same source and extracts
// the body the driver prompt carried, so the tool path can be byte-compared
// against it.
func slashInjectedBody(t *testing.T, source chat.SkillSource) string {
	t.Helper()

	driver := &scriptedDriver{turns: []runner.Result{finalTurn(plainAnswer)}}
	agent := newSkillAgent(t, driver, source)
	sessionID := newSession(t, agent)

	if _, err := sendMessage(t, agent, sessionID, "/"+skillNameBig+" load it"); err != nil {
		t.Fatalf("send message: %v", err)
	}

	prompt := driver.lastPrompt(t)

	prefix := "Skill: " + skillNameBig + "\n"

	start := strings.Index(prompt, prefix)
	if start < 0 {
		t.Fatalf("slash prompt missing the skill section:\n%s", head(prompt, 120))
	}

	injected, _, found := strings.Cut(prompt[start+len(prefix):], "\n\nConversation so far:")
	if !found {
		t.Fatalf("slash prompt skill section not terminated:\n%s", head(prompt, 120))
	}

	return injected
}

// TestSkillToolUnknownNamePersistsError proves an unknown name persists the
// skill.not_found error value rather than a raw-body record.
func TestSkillToolUnknownNamePersistsError(t *testing.T) {
	t.Parallel()

	source := &fakeSkillSource{bodies: map[string]string{}}
	driver := &scriptedDriver{turns: []runner.Result{
		toolTurn(chat.SkillToolName, map[string]any{"name": "ghost"}),
		finalTurn(plainAnswer),
	}}
	agent := newSkillToolAgent(t, driver, source)
	sessionID := newSession(t, agent)

	if _, err := sendMessage(t, agent, sessionID, "load ghost"); err != nil {
		t.Fatalf("send message: %v", err)
	}

	record := onlyToolRecord(t, agent, sessionID)
	if !strings.Contains(record, "skill.not_found") {
		t.Errorf("unknown-skill record missing the skill.not_found code:\n%s", record)
	}

	if strings.Contains(record, "body:") {
		t.Errorf("unknown-skill record took the raw-body path:\n%s", record)
	}
}

// TestNonSkillToolResultTruncates proves a non-skill tool result over the cap
// still clips, so the skill exemption does not leak to other tools.
func TestNonSkillToolResultTruncates(t *testing.T) {
	t.Parallel()

	driver := &scriptedDriver{turns: []runner.Result{
		toolTurn("bigtool", map[string]any{}),
		finalTurn(plainAnswer),
	}}

	db := openStore(t)
	agent := chat.New(&chat.Config{
		Store:         db,
		Driver:        driver,
		Tools:         map[string]runner.Runner{"bigtool": &bigResultTool{output: strings.Repeat("x", 5000)}},
		Personalities: chat.NewPersonalities(nil, ""),
		NewID:         sqlite.NewID,
	})
	sessionID := newSession(t, agent)

	if _, err := sendMessage(t, agent, sessionID, "call bigtool"); err != nil {
		t.Fatalf("send message: %v", err)
	}

	record := onlyToolRecord(t, agent, sessionID)
	if !strings.Contains(record, truncatedMarker) {
		t.Errorf("non-skill tool record over the cap was not truncated:\n%s", head(record, 200))
	}
}
