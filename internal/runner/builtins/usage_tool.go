package builtins

import (
	"encoding/json"
	"strings"

	"github.com/chadit/CairnOS/pkg/runner"
)

// usageToolDescription tells a chat model what the probe returns and asks it to
// pass the table through, since a paraphrased table loses the reset times.
const usageToolDescription = "Report how much of each configured Claude Code and Codex login's rate limits is used: " +
	"the 5-hour and weekly windows with the share used and the reset time, as a Markdown table. " +
	"Read-only and spends no quota. Put the table in your reply verbatim. " +
	"Several Claude logins take about a minute each, because the probe waits out the CLI's usage cache between them."

// Describe implements runner.Describer, which is what lets the chat agent offer
// the probe as a read tool. Both params are optional, so an empty call probes
// every configured launcher on this machine.
func (*Usage) Describe() runner.ToolSpec {
	return runner.ToolSpec{
		Name:        UsageName,
		Description: usageToolDescription,
		InputSchema: map[string]any{
			schemaType: schemaObject,
			schemaProperties: map[string]any{
				paramUsageHost: map[string]any{
					schemaType: schemaString,
					schemaDesc: "a configured host name to probe over ssh; omit for this machine",
				},
				paramUsageLaunchers: map[string]any{
					schemaType:  schemaArray,
					schemaItems: map[string]any{schemaType: schemaString},
					schemaDesc:  "configured launcher names to narrow the probe to; omit for every login",
				},
			},
		},
	}
}

// UsageToolText renders an Execute output as the text a chat model reads. The
// JSON form escapes the table's newlines, which invites the model to rebuild it.
func UsageToolText(output any) (string, bool) {
	encoded, err := json.Marshal(output)
	if err != nil {
		return "", false
	}

	var report UsageReport

	if err := json.Unmarshal(encoded, &report); err != nil || report.Summary == "" {
		return "", false
	}

	return report.toolText(), true
}

// toolText is the table, plus one line naming the failed logins so the model
// can call them out without reading every row.
func (r UsageReport) toolText() string {
	if !r.HasErrors {
		return r.Summary
	}

	failed := make([]string, 0, len(r.Logins))

	for _, login := range r.Logins {
		if login.Error != "" {
			failed = append(failed, login.Launcher)
		}
	}

	return r.Summary + "\nfailed logins: " + strings.Join(failed, ", ")
}
