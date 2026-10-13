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

// usageReportToolDescription replaces it when [runner.usage] reads a script's
// report: the call is instant, and a stale warning must reach the reply.
const usageReportToolDescription = "Report how much of each Claude Code and Codex account's rate limits is used, " +
	"from the latest report the usage script wrote: the 5-hour and weekly windows with the share used and the reset time, " +
	"plus each account's ranking score and banked reset credits, as a Markdown table. " +
	"Read-only and spends no quota. Put the table in your reply verbatim, with the stale warning above it when there is one."

// Describe implements runner.Describer, which is what lets the chat agent offer
// the probe as a read tool. Both params are optional, so an empty call probes
// every configured launcher on this machine. Reading a script's report offers
// launchers alone: the file lives where the script runs, so there is no host to
// pick.
func (u *Usage) Describe() runner.ToolSpec {
	if u.script != nil {
		return usageToolSpec(usageReportToolDescription, map[string]any{
			paramUsageLaunchers: usageLaunchersProperty("launcher names from the report to narrow it to; omit for every account"),
		})
	}

	return usageToolSpec(usageToolDescription, map[string]any{
		paramUsageHost: map[string]any{
			schemaType: schemaString,
			schemaDesc: "a configured host name to probe over ssh; omit for this machine",
		},
		paramUsageLaunchers: usageLaunchersProperty("configured launcher names to narrow the probe to; omit for every login"),
	})
}

// usageToolSpec wraps one mode's description and params in the tool shape.
func usageToolSpec(description string, properties map[string]any) runner.ToolSpec {
	return runner.ToolSpec{
		Name:        UsageName,
		Description: description,
		InputSchema: map[string]any{
			schemaType:       schemaObject,
			schemaProperties: properties,
		},
	}
}

// usageLaunchersProperty is the launchers param, a list of names.
func usageLaunchersProperty(description string) map[string]any {
	return map[string]any{
		schemaType:  schemaArray,
		schemaItems: map[string]any{schemaType: schemaString},
		schemaDesc:  description,
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
func (r *UsageReport) toolText() string {
	if !r.HasErrors {
		return r.Summary
	}

	failed := make([]string, 0, len(r.Logins))

	for index := range r.Logins {
		if login := &r.Logins[index]; login.Error != "" {
			failed = append(failed, login.Launcher)
		}
	}

	return r.Summary + "\nfailed logins: " + strings.Join(failed, ", ")
}
