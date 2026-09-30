package chat_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/chat"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/pkg/runner"
)

// usageTable is the fenced summary a probe of one working and one refused login
// renders, the text the reply has to carry unchanged.
const usageTable = "```\n" +
	"| LOGIN        | PLAN    | WINDOW | USED | RESETS               |\n" +
	"|--------------|---------|--------|------|----------------------|\n" +
	"| claude-agent |         | error: login expired                 |\n" +
	"| codex-work   | prolite | 7d     | 24%  | 2026-09-26 10:01 UTC |\n" +
	"```"

// cannedUsage stands in for the usage runner with the output shape its Execute
// returns, so the chat path is exercised without launching any CLI.
type cannedUsage struct {
	output map[string]any
}

var _ runner.Describer = (*cannedUsage)(nil)

func (*cannedUsage) Name() string                { return chat.ToolUsage }
func (*cannedUsage) Validate(*runner.Task) error { return nil }
func (c *cannedUsage) Execute(context.Context, *runner.Task) (runner.Result, error) {
	return runner.Result{Output: c.output}, nil
}
func (*cannedUsage) Describe() runner.ToolSpec { return runner.ToolSpec{Name: chat.ToolUsage} }

// newCannedUsage renders a report with one refused login through the runner's
// own Map, so the chat side reads exactly what a real probe hands back.
func newCannedUsage(t *testing.T) *cannedUsage {
	t.Helper()

	output, err := (&builtins.UsageReport{
		Summary:   usageTable,
		HasErrors: true,
		Logins: []builtins.UsageLogin{
			{Launcher: "claude-agent", Protocol: "claude", Error: "login expired"},
			{Launcher: "codex-work", Protocol: "codex", Plan: "prolite"},
		},
	}).Map()
	if err != nil {
		t.Fatalf("render report: %v", err)
	}

	return &cannedUsage{output: output}
}

// specNames lists the descriptor names in the order the driver sees them.
func specNames(set chat.ToolSet) []string {
	names := make([]string, len(set.Specs))
	for idx := range set.Specs {
		names[idx] = set.Specs[idx].Name
	}

	return names
}

func TestBuildToolSetListsUsageAfterSkill(t *testing.T) {
	t.Parallel()

	set := chat.BuildToolSet(map[string]runner.Runner{
		chat.ToolUsage:     newCannedUsage(t),
		chat.SkillToolName: chat.NewSkillTool(&fakeSkillSource{}),
	})

	want := []string{chat.SkillToolName, chat.ToolUsage, chat.RunWorkflowToolName}
	if got := specNames(set); !slices.Equal(got, want) {
		t.Fatalf("spec order = %v, want %v", got, want)
	}

	if _, present := set.Tools[chat.ToolUsage]; !present {
		t.Fatalf("tools = %v, want %s executable", set.Tools, chat.ToolUsage)
	}
}

func TestBuildToolSetSkipsANilUsageRunner(t *testing.T) {
	t.Parallel()

	set := chat.BuildToolSet(map[string]runner.Runner{chat.ToolUsage: nil})

	want := []string{chat.RunWorkflowToolName}
	if got := specNames(set); !slices.Equal(got, want) {
		t.Fatalf("spec order = %v, want %v", got, want)
	}

	if _, present := set.Tools[chat.ToolUsage]; present {
		t.Fatalf("tools carry %s although its runner was nil", chat.ToolUsage)
	}
}

func TestUsageToolFeedsTheTableBackUnescaped(t *testing.T) {
	t.Parallel()

	reply := "Here is where the logins stand:\n" + usageTable
	driver := &scriptedDriver{turns: []runner.Result{
		toolTurn(chat.ToolUsage, map[string]any{}),
		finalTurn(reply),
	}}

	db := openStore(t)
	set := chat.BuildToolSet(map[string]runner.Runner{chat.ToolUsage: newCannedUsage(t)})
	agent := chat.New(&chat.Config{
		Store:         db,
		Driver:        driver,
		Tools:         set.Tools,
		Specs:         set.Specs,
		Personalities: chat.NewPersonalities(nil, ""),
		NewID:         sqlite.NewID,
	})
	sessionID := newSession(t, agent)

	if _, err := sendMessage(t, agent, sessionID, "usage"); err != nil {
		t.Fatalf("send message: %v", err)
	}

	// The table has to reach the driver with real newlines, or the model sees
	// JSON escapes and rebuilds the table instead of quoting it.
	prompt := driver.lastPrompt(t)
	if !strings.Contains(prompt, usageTable) {
		t.Fatalf("second prompt lacks the verbatim table; got:\n%s", prompt)
	}

	if !strings.Contains(prompt, "failed logins: claude-agent") {
		t.Fatalf("second prompt lacks the failed-login line; got:\n%s", prompt)
	}

	if strings.Contains(prompt, `"has_errors"`) {
		t.Fatalf("second prompt carries the raw JSON report; got:\n%s", prompt)
	}

	assertMessageRoles(t, db, sessionID, []string{store.ChatRoleUser, store.ChatRoleTool, store.ChatRoleAssistant})
	assertLastAssistant(t, db, sessionID, reply)
}
