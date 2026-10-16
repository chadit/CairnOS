package poll

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"time"

	"github.com/chadit/CairnOS/pkg/runner"
)

// GitHubSourceName is the [workflow.poll] source name the GitHub issues adapter
// registers under.
const GitHubSourceName = "github_issues"

// The list_issues op name plus the param and output keys the adapter shares
// with the github builtin runner. This is the wire contract between the source
// and the op it drives, the same key vocabulary a workflow file uses to reach a
// runner.
const (
	listIssuesOp = "list_issues"
	opParam      = "op"
	sinceParam   = "since"

	keyNumber    = "number"
	keyTitle     = "title"
	keyBody      = "body"
	keyURL       = "url"
	keyAuthor    = "author"
	keyCreatedAt = "created_at"
)

// issueRunner is the slice of runner.Runner GitHubIssues drives: one Execute
// per poll. The registry wiring hands in the github builtin; tests hand in the
// same builtin pointed at an httptest server.
type issueRunner interface {
	Execute(ctx context.Context, task *runner.Task) (runner.Result, error)
}

// GitHubIssues adapts the github builtin runner's list_issues op to Source. The
// cursor is the latest issue created_at seen (RFC3339); a poll passes it as the
// op's since so only newer issues come back, and advances it to the newest
// created_at observed (.agents/spec/parity/triggers-webhooks.md, "Sources").
type GitHubIssues struct {
	runner issueRunner
}

// NewGitHubIssues wraps a runner (the github builtin) as a poll Source.
func NewGitHubIssues(driver issueRunner) *GitHubIssues {
	return &GitHubIssues{runner: driver}
}

// Poll returns issues created since cursor and advances the cursor to the newest
// created_at observed. An empty cursor is the first poll: it emits no events and
// sets the cursor to now, so registering a poll workflow never backfills a
// repo's whole issue history. The op itself drops PR rows and re-checks
// created_at, since GitHub's since filter matches updated_at.
func (g *GitHubIssues) Poll(ctx context.Context, params map[string]any, cursor string) ([]Event, string, error) {
	if cursor == "" {
		return nil, time.Now().UTC().Format(time.RFC3339), nil
	}

	task := &runner.Task{Params: issueParams(params, cursor), Resolved: true}

	result, err := g.runner.Execute(ctx, task)
	if err != nil {
		return nil, "", fmt.Errorf("poll %s: %w", GitHubSourceName, err)
	}

	items, ok := result.Output.([]any)
	if !ok {
		return nil, "", fmt.Errorf("%w: list_issues output was %T", ErrBadResult, result.Output)
	}

	events := make([]Event, 0, len(items))
	next := cursor

	for _, item := range items {
		fields, isMap := item.(map[string]any)
		if !isMap {
			continue
		}

		event, createdAt := eventFrom(fields)
		events = append(events, event)

		if laterRFC3339(createdAt, next) {
			next = createdAt
		}
	}

	return events, next, nil
}

// stampedParamCount sizes the merged params for the op and since keys
// issueParams adds.
const stampedParamCount = 2

// issueParams copies the workflow's poll params and stamps the op and the since
// cursor, leaving the caller's map untouched.
func issueParams(params map[string]any, cursor string) map[string]any {
	merged := make(map[string]any, len(params)+stampedParamCount)
	maps.Copy(merged, params)

	merged[opParam] = listIssuesOp
	merged[sinceParam] = cursor

	return merged
}

// eventFrom projects one list_issues output row onto an Event: the issue number
// is the stable dedup id, and the six declared input values ride in Inputs.
// BuiltinSourceInputs lists the same keys for add-time checks, so a key added
// here goes there too.
func eventFrom(fields map[string]any) (Event, string) {
	createdAt, _ := fields[keyCreatedAt].(string)

	inputs := map[string]any{
		keyNumber:    fields[keyNumber],
		keyTitle:     fields[keyTitle],
		keyBody:      fields[keyBody],
		keyURL:       fields[keyURL],
		keyAuthor:    fields[keyAuthor],
		keyCreatedAt: createdAt,
	}

	return Event{ID: issueID(fields[keyNumber]), Inputs: inputs}, createdAt
}

// issueID renders the issue number as a stable string id across the int64 the
// op emits and the float64 a JSON round-trip would leave.
func issueID(number any) string {
	switch value := number.(type) {
	case int64:
		return strconv.FormatInt(value, 10)
	case int:
		return strconv.Itoa(value)
	case float64:
		return strconv.FormatInt(int64(value), 10)
	default:
		return fmt.Sprint(value)
	}
}

// laterRFC3339 reports whether candidate is a strictly newer RFC3339 timestamp
// than current. An unparseable candidate never advances the cursor; an
// unparseable current reads as no lower bound, so any parseable candidate wins.
func laterRFC3339(candidate, current string) bool {
	newer, err := time.Parse(time.RFC3339, candidate)
	if err != nil {
		return false
	}

	baseline, err := time.Parse(time.RFC3339, current)
	if err != nil {
		return true
	}

	return newer.After(baseline)
}
