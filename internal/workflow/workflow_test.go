package workflow_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/workflow"
)

const fullWorkflow = `
[workflow]
name        = "career_scout"
description = "Find remote AI jobs and rank them."
schedule    = "@daily"
on_error    = "continue"
notify      = ["email:me@example.com"]
timeout     = "30m"

[workflow.inputs.region]
type        = "string"
required    = true
description = "Target metro area."

[workflow.inputs.max_results]
type    = "int"
default = 20

[action.search_jobs]
runner = "mock"
params = { query = "remote AI agent developer jobs", region = "${{ inputs.region }}" }

[action.score_jobs]
runner     = "mock"
depends_on = ["search_jobs"]
idempotent = true
params     = { jobs = "${{ search_jobs.output }}" }

[action.score_jobs.retry]
max_attempts = 3
base_delay   = "1s"
max_delay    = "30s"

[action.notify_me]
runner     = "mock"
depends_on = ["score_jobs"]
condition  = "${{ score_jobs.output.count }} > 0"
params     = { body = "${{ score_jobs.output }}" }

[action.investigate]
kind           = "agent"
driver         = "mock"
goal           = "Summarize the day's findings for ${{ inputs.region }}."
tools          = ["memory"]
budget_usd     = 1.5
depends_on     = ["score_jobs"]
`

func TestParseAcceptsFullWorkflow(t *testing.T) {
	t.Parallel()

	flow, err := workflow.Parse([]byte(fullWorkflow))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if flow.Name != "career_scout" || flow.OnError != workflow.OnErrorContinue {
		t.Fatalf("meta = %q/%q, want career_scout/continue", flow.Name, flow.OnError)
	}

	if flow.Timeout != 30*time.Minute {
		t.Fatalf("timeout = %v, want 30m", flow.Timeout)
	}

	if len(flow.Actions) != 4 {
		t.Fatalf("got %d actions, want 4", len(flow.Actions))
	}

	score := flow.Actions["score_jobs"]
	if score.Retry == nil || score.Retry.MaxAttempts != 3 || !score.Retry.Jitter {
		t.Fatalf("retry = %+v, want max_attempts 3 with jitter defaulted on", score.Retry)
	}

	if notify := flow.Actions["notify_me"]; notify.CondExpr == nil {
		t.Fatal("condition was not parsed into an expression")
	}

	agent := flow.Actions["investigate"]
	if agent.Kind != workflow.KindAgent || agent.MaxIterations != 10 {
		t.Fatalf("agent = kind %q iterations %d, want agent/10 (default)", agent.Kind, agent.MaxIterations)
	}

	input := flow.Inputs["max_results"]
	if !input.HasDefault || input.Type != workflow.TypeInt {
		t.Fatalf("input max_results = %+v, want int with default", input)
	}
}

func TestParseAcceptsActionFallback(t *testing.T) {
	t.Parallel()

	const toml = "[workflow]\nname = \"w\"\n[action.work]\nrunner = \"primary\"\nfallback = [\"second\", \"third\"]"

	flow, err := workflow.Parse([]byte(toml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	got := flow.Actions["work"].Fallback
	if want := []string{"second", "third"}; !slices.Equal(got, want) {
		t.Fatalf("fallback = %v, want %v", got, want)
	}
}

func TestParseAcceptsForeach(t *testing.T) {
	t.Parallel()

	const toml = `[workflow]
name = "w"
[action.fetch]
runner = "mock"
[action.score]
runner = "mock"
depends_on = ["fetch"]
params = { body = "${{ item }}", tag = "${{ index }}" }
[action.score.foreach]
items = "${{ fetch.output.items }}"
max_parallel = 2`

	flow, err := workflow.Parse([]byte(toml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	spec := flow.Actions["score"].Foreach
	if spec == nil {
		t.Fatal("foreach was not parsed")
	}

	if spec.MaxParallel != 2 {
		t.Errorf("max_parallel = %d, want 2", spec.MaxParallel)
	}

	got, isString := spec.Items.(string)
	if !isString || got != "${{ fetch.output.items }}" {
		t.Errorf("items = %v, want the whole reference string", spec.Items)
	}
}

func TestParseAcceptsWorkflowCall(t *testing.T) {
	t.Parallel()

	const toml = `[workflow]
name = "parent"
[action.fetch]
runner = "mock"
[action.enrich]
kind = "workflow"
workflow = "enrich_contact"
depends_on = ["fetch"]
[action.enrich.inputs]
email = "${{ fetch.output.email }}"
mode = "full"`

	flow, err := workflow.Parse([]byte(toml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	enrich := flow.Actions["enrich"]
	if enrich.Kind != workflow.KindWorkflow {
		t.Fatalf("kind = %q, want %q", enrich.Kind, workflow.KindWorkflow)
	}

	if enrich.Workflow != "enrich_contact" {
		t.Errorf("workflow = %q, want enrich_contact", enrich.Workflow)
	}

	if enrich.Inputs["email"] != "${{ fetch.output.email }}" || enrich.Inputs["mode"] != "full" {
		t.Errorf("inputs = %v, want the email ref and mode literal", enrich.Inputs)
	}
}

func TestParseAcceptsErrorWorkflow(t *testing.T) {
	t.Parallel()

	const toml = `[workflow]
name = "sync_crm"
error_workflow = "on_sync_failure"
[action.a]
runner = "mock"`

	flow, err := workflow.Parse([]byte(toml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if flow.ErrorWorkflow != "on_sync_failure" {
		t.Fatalf("error_workflow = %q, want on_sync_failure", flow.ErrorWorkflow)
	}
}

func TestParseForeachDefaultsMaxParallelToOne(t *testing.T) {
	t.Parallel()

	const toml = `[workflow]
name = "w"
[action.a]
runner = "mock"
params = { v = "${{ item }}" }
[action.a.foreach]
items = ["x", "y"]`

	flow, err := workflow.Parse([]byte(toml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	spec := flow.Actions["a"].Foreach
	if spec == nil {
		t.Fatal("foreach was not parsed")
	}

	if spec.MaxParallel != 1 {
		t.Errorf("max_parallel = %d, want the default 1 when the key is absent", spec.MaxParallel)
	}

	if _, isArray := spec.Items.([]any); !isArray {
		t.Errorf("items = %T, want a literal array", spec.Items)
	}
}

func TestExtractRefsClassifiesForeachNamespaces(t *testing.T) {
	t.Parallel()

	refs, errs := workflow.ExtractRefs("a=${{ item }} b=${{ item.email }} c=${{ index }}")
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	wantKinds := []workflow.RefKind{workflow.RefItem, workflow.RefItem, workflow.RefIndex}
	if len(refs) != len(wantKinds) {
		t.Fatalf("got %d refs, want %d", len(refs), len(wantKinds))
	}

	for idx, ref := range refs {
		if ref.Kind != wantKinds[idx] {
			t.Errorf("ref %d kind = %v, want %v", idx, ref.Kind, wantKinds[idx])
		}
	}

	if len(refs[1].Path) != 1 || refs[1].Path[0] != "email" {
		t.Errorf("item field path = %v, want [email]", refs[1].Path)
	}
}

func TestParseAcceptsBudget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		toml       string
		wantPeriod string
		wantExceed string
		wantLimit  float64
	}{
		{
			name:       "pause on month",
			toml:       "[workflow]\nname = \"w\"\n[workflow.budget]\nperiod = \"month\"\nlimit_usd = 50.0\non_exceed = \"pause\"\n[action.a]\nrunner = \"mock\"",
			wantPeriod: workflow.BudgetPeriodMonth,
			wantLimit:  50.0,
			wantExceed: workflow.BudgetOnExceedPause,
		},
		{
			name:       "warn on day with integer limit",
			toml:       "[workflow]\nname = \"w\"\n[workflow.budget]\nperiod = \"day\"\nlimit_usd = 5\non_exceed = \"warn\"\n[action.a]\nrunner = \"mock\"",
			wantPeriod: workflow.BudgetPeriodDay,
			wantLimit:  5.0,
			wantExceed: workflow.BudgetOnExceedWarn,
		},
		{
			name:       "pause on week",
			toml:       "[workflow]\nname = \"w\"\n[workflow.budget]\nperiod = \"week\"\nlimit_usd = 12.5\non_exceed = \"pause\"\n[action.a]\nrunner = \"mock\"",
			wantPeriod: workflow.BudgetPeriodWeek,
			wantLimit:  12.5,
			wantExceed: workflow.BudgetOnExceedPause,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			flow, err := workflow.Parse([]byte(tt.toml))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			if flow.Budget == nil {
				t.Fatal("budget was not parsed")
			}

			if flow.Budget.Period != tt.wantPeriod {
				t.Errorf("period = %q, want %q", flow.Budget.Period, tt.wantPeriod)
			}

			if flow.Budget.LimitUSD != tt.wantLimit {
				t.Errorf("limit_usd = %v, want %v", flow.Budget.LimitUSD, tt.wantLimit)
			}

			if flow.Budget.OnExceed != tt.wantExceed {
				t.Errorf("on_exceed = %q, want %q", flow.Budget.OnExceed, tt.wantExceed)
			}
		})
	}
}

func TestParseAcceptsPoll(t *testing.T) {
	t.Parallel()

	const src = `
[workflow]
name     = "gh_issue_triage"
schedule = "*/5 * * * *"

[workflow.poll]
source = "github_issues"

[workflow.poll.params]
repo  = "octocat/hello-world"
state = "open"

[action.triage]
runner = "mock"
`

	flow, err := workflow.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if flow.Poll == nil {
		t.Fatal("poll was not parsed")
	}

	if flow.Poll.Source != "github_issues" {
		t.Errorf("source = %q, want github_issues", flow.Poll.Source)
	}

	if flow.Poll.Params["repo"] != "octocat/hello-world" || flow.Poll.Params["state"] != "open" {
		t.Errorf("params = %#v, want repo/state passed through", flow.Poll.Params)
	}
}

func TestParseLeavesPollNilWhenAbsent(t *testing.T) {
	t.Parallel()

	flow, err := workflow.Parse([]byte("[workflow]\nname = \"w\"\n[action.a]\nrunner = \"mock\""))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if flow.Poll != nil {
		t.Fatalf("poll = %+v, want nil when no [workflow.poll] table", flow.Poll)
	}
}

// TestParseAcceptsSecondsSchedule covers the six-field form a sub-minute
// cadence needs. Field ranges and the fire-rate floor belong to the scheduler,
// so loading the file only has to stop rejecting the extra field.
func TestParseAcceptsSecondsSchedule(t *testing.T) {
	t.Parallel()

	const src = "[workflow]\nname = \"w\"\nschedule = \"*/30 * * * * *\"\n[action.a]\nrunner = \"mock\""

	flow, err := workflow.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if flow.Schedule != "*/30 * * * * *" {
		t.Fatalf("schedule = %q, want the six-field expression unchanged", flow.Schedule)
	}
}

func TestParseAcceptsSystemHostname(t *testing.T) {
	t.Parallel()

	const src = "[workflow]\nname = \"w\"\n[action.a]\nrunner = \"mock\"\n" +
		"params = { key = \"usage:${{ system.hostname }}\" }"

	if _, err := workflow.Parse([]byte(src)); err != nil {
		t.Fatalf("parse: %v", err)
	}

	refs, errs := workflow.ExtractRefs("${{ system.hostname }}")
	if len(errs) > 0 || len(refs) != 1 || refs[0].Kind != workflow.RefSystem || refs[0].Target != workflow.SystemHostname {
		t.Fatalf("refs = %+v, errs = %v, want one system ref to the hostname", refs, errs)
	}
}

func TestParseRejectsUnknownSystemValue(t *testing.T) {
	t.Parallel()

	const src = "[workflow]\nname = \"w\"\n[action.a]\nrunner = \"mock\"\nparams = { x = \"${{ system.kernel }}\" }"

	_, err := workflow.Parse([]byte(src))
	if err == nil {
		t.Fatal("parse succeeded, want system.kernel refused")
	}

	if codes := codesOf(t, err); !slices.Contains(codes, workflow.CodeBadReference) {
		t.Fatalf("codes = %v, want %s", codes, workflow.CodeBadReference)
	}
}

func TestParseAcceptsRequires(t *testing.T) {
	t.Parallel()

	const src = `
[workflow]
name     = "prflow_app"
schedule = "* * * * *"

[workflow.requires]
urls = ["https://git.example.com/team/app"]
vpn  = "tun0"

[action.reconcile]
runner = "mock"
`

	flow, err := workflow.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if flow.Requires == nil {
		t.Fatal("requires was not parsed")
	}

	if len(flow.Requires.URLs) != 1 || flow.Requires.URLs[0] != "https://git.example.com/team/app" {
		t.Errorf("urls = %q, want the one repository URL", flow.Requires.URLs)
	}

	if flow.Requires.VPN != "tun0" {
		t.Errorf("vpn = %q, want tun0", flow.Requires.VPN)
	}
}

func TestParseLeavesRequiresNilWhenAbsent(t *testing.T) {
	t.Parallel()

	flow, err := workflow.Parse([]byte("[workflow]\nname = \"w\"\n[action.a]\nrunner = \"mock\""))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if flow.Requires != nil {
		t.Fatalf("requires = %+v, want nil when no [workflow.requires] table", flow.Requires)
	}
}

func TestParseRejectsBadRequires(t *testing.T) {
	t.Parallel()

	// A wrong TOML type is caught by the shared scalar decoder, so it reports
	// workflow.invalid; every rule the requires validator owns reports
	// workflow.bad_requires.
	cases := map[string]struct {
		table    string
		wantCode string
	}{
		"no urls":          {"[workflow.requires]\nvpn = \"tun0\"\n", workflow.CodeBadRequires},
		"relative url":     {"[workflow.requires]\nurls = [\"git.example.com/repo\"]\n", workflow.CodeBadRequires},
		"unsupported":      {"[workflow.requires]\nurls = [\"ssh://git.example.com/repo\"]\n", workflow.CodeBadRequires},
		"unknown key":      {"[workflow.requires]\nurls = [\"https://git.example.com\"]\nping = true\n", workflow.CodeBadRequires},
		"vpn not a string": {"[workflow.requires]\nurls = [\"https://git.example.com\"]\nvpn = 1\n", workflow.CodeInvalid},
	}

	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			src := "[workflow]\nname = \"w\"\n" + tt.table + "[action.a]\nrunner = \"mock\"\n"

			_, err := workflow.Parse([]byte(src))
			if err == nil {
				t.Fatalf("parse succeeded, want a %s error", tt.wantCode)
			}

			if codes := codesOf(t, err); !slices.Contains(codes, tt.wantCode) {
				t.Fatalf("codes = %v, want %s", codes, tt.wantCode)
			}
		})
	}
}

func TestParseLeavesBudgetNilWhenAbsent(t *testing.T) {
	t.Parallel()

	flow, err := workflow.Parse([]byte("[workflow]\nname = \"w\"\n[action.a]\nrunner = \"mock\""))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if flow.Budget != nil {
		t.Fatalf("budget = %+v, want nil when no [workflow.budget] table", flow.Budget)
	}
}

func TestLoadReadsWorkflowFromDisk(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "flow.toml")
	if err := os.WriteFile(path, []byte(fullWorkflow), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	flow, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if flow.Name != "career_scout" {
		t.Fatalf("name = %q", flow.Name)
	}

	sum := sha256.Sum256([]byte(fullWorkflow))
	if want := hex.EncodeToString(sum[:]); flow.Hash != want {
		t.Fatalf("hash = %q, want %q", flow.Hash, want)
	}
}

func TestParseLeavesHashEmpty(t *testing.T) {
	t.Parallel()

	flow, err := workflow.Parse([]byte(fullWorkflow))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if flow.Hash != "" {
		t.Fatalf("hash = %q, want empty: raw bytes carry no file identity", flow.Hash)
	}
}

// codesOf unwraps the aggregated validation error into its canonical codes.
func codesOf(t *testing.T, err error) []string {
	t.Helper()

	list, ok := errors.AsType[cairnerr.Errors](err)
	if !ok {
		t.Fatalf("error is %T, want cairnerr.Errors", err)
	}

	codes := make([]string, 0, len(list))
	for _, item := range list {
		codes = append(codes, item.Code)
	}

	return codes
}

// retryAction wraps one retry table in the smallest workflow that carries it,
// so a case shows only the fields it is about.
func retryAction(retryBody string) string {
	return "[workflow]\nname = \"w\"\n[action.a]\nrunner = \"mock\"\n[action.a.retry]\n" + retryBody + "\n"
}

// agentAction wraps extra agent keys in an otherwise valid agent action, so a
// case's own field is the only thing the validator can object to.
func agentAction(extra string) string {
	return "[workflow]\nname = \"w\"\n[action.a]\nkind = \"agent\"\ndriver = \"mock\"\n" +
		"goal = \"g\"\ntools = []\nbudget_usd = 1.0\n" + extra + "\n"
}

func TestParseRejectsRuleViolations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		toml     string
		wantCode string
	}{
		{
			name:     "missing workflow table",
			toml:     `[action.a]` + "\n" + `runner = "mock"`,
			wantCode: workflow.CodeInvalid,
		},
		{
			name:     "bad workflow name",
			toml:     "[workflow]\nname = \"Bad-Name\"",
			wantCode: workflow.CodeBadName,
		},
		{
			name:     "unknown dependency",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nrunner = \"mock\"\ndepends_on = [\"ghost\"]",
			wantCode: workflow.CodeUnknownDependency,
		},
		{
			name: "dependency cycle",
			toml: "[workflow]\nname = \"w\"\n" +
				"[action.a]\nrunner = \"mock\"\ndepends_on = [\"b\"]\n" +
				"[action.b]\nrunner = \"mock\"\ndepends_on = [\"a\"]",
			wantCode: workflow.CodeCycleDetected,
		},
		{
			name: "reference without dependency",
			toml: "[workflow]\nname = \"w\"\n" +
				"[action.a]\nrunner = \"mock\"\n" +
				"[action.b]\nrunner = \"mock\"\nparams = { x = \"${{ a.output }}\" }",
			wantCode: workflow.CodeBadReference,
		},
		{
			name:     "reference to unknown action",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nrunner = \"mock\"\nparams = { x = \"${{ ghost.output }}\" }",
			wantCode: workflow.CodeBadReference,
		},
		{
			name:     "undeclared input",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nrunner = \"mock\"\nparams = { x = \"${{ inputs.ghost }}\" }",
			wantCode: workflow.CodeUnknownInput,
		},
		{
			name:     "default type mismatch",
			toml:     "[workflow]\nname = \"w\"\n[workflow.inputs.n]\ntype = \"int\"\ndefault = \"twenty\"\n[action.a]\nrunner = \"mock\"",
			wantCode: workflow.CodeBadDefault,
		},
		{
			name:     "bad condition grammar",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nrunner = \"mock\"\ncondition = \"1 ==\"",
			wantCode: workflow.CodeBadCondition,
		},
		{
			name:     "bad duration",
			toml:     "[workflow]\nname = \"w\"\ntimeout = \"soon\"\n[action.a]\nrunner = \"mock\"",
			wantCode: workflow.CodeBadDuration,
		},
		{
			name:     "rollback reserved",
			toml:     "[workflow]\nname = \"w\"\non_error = \"rollback\"\n[action.a]\nrunner = \"mock\"",
			wantCode: workflow.CodeReserved,
		},
		{
			name:     "foreach not a table",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nrunner = \"mock\"\nforeach = \"items\"",
			wantCode: workflow.CodeBadForeach,
		},
		{
			name:     "action budget reserved",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nrunner = \"mock\"\nbudget = \"x\"",
			wantCode: workflow.CodeReserved,
		},
		{
			name: "foreach on agent kind",
			toml: `[workflow]
name = "w"
[action.a]
kind = "agent"
driver = "m"
goal = "g"
tools = []
budget_usd = 1.0
[action.a.foreach]
items = ["x"]`,
			wantCode: workflow.CodeBadForeach,
		},
		{
			name:     "workflow-call missing workflow",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nkind = \"workflow\"",
			wantCode: workflow.CodeInvalid,
		},
		{
			name:     "workflow-call bad workflow name",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nkind = \"workflow\"\nworkflow = \"Bad-Name\"",
			wantCode: workflow.CodeBadName,
		},
		{
			name:     "workflow-call with runner",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nkind = \"workflow\"\nworkflow = \"child\"\nrunner = \"mock\"",
			wantCode: workflow.CodeInvalid,
		},
		{
			name: "secret ref in workflow-call inputs",
			toml: `[workflow]
name = "w"
[action.a]
kind = "workflow"
workflow = "child"
[action.a.inputs]
token = "${{ secrets.api_key }}"`,
			wantCode: workflow.CodeSecretNotAllowed,
		},
		{
			name: "workflow-call inputs reference undeclared dependency",
			toml: `[workflow]
name = "w"
[action.up]
runner = "mock"
[action.a]
kind = "workflow"
workflow = "child"
[action.a.inputs]
v = "${{ up.output }}"`,
			wantCode: workflow.CodeBadReference,
		},
		{
			name:     "task with workflow-call keys",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nrunner = \"mock\"\nworkflow = \"child\"",
			wantCode: workflow.CodeInvalid,
		},
		{
			name: "foreach max_parallel below one",
			toml: `[workflow]
name = "w"
[action.a]
runner = "mock"
[action.a.foreach]
items = ["x"]
max_parallel = 0`,
			wantCode: workflow.CodeBadForeach,
		},
		{
			name: "foreach missing items",
			toml: `[workflow]
name = "w"
[action.a]
runner = "mock"
[action.a.foreach]
max_parallel = 2`,
			wantCode: workflow.CodeBadForeach,
		},
		{
			name:     "item ref outside foreach",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nrunner = \"mock\"\nparams = { x = \"${{ item }}\" }",
			wantCode: workflow.CodeBadForeach,
		},
		{
			name:     "index ref outside foreach",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nrunner = \"mock\"\nparams = { x = \"${{ index }}\" }",
			wantCode: workflow.CodeBadForeach,
		},
		{
			name: "foreach items references undeclared dependency",
			toml: `[workflow]
name = "w"
[action.up]
runner = "mock"
[action.a]
runner = "mock"
[action.a.foreach]
items = "${{ up.output }}"`,
			wantCode: workflow.CodeBadReference,
		},
		{
			name:     "agent missing required fields",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nkind = \"agent\"\ndriver = \"mock\"",
			wantCode: workflow.CodeAgentFields,
		},
		{
			name:     "task with agent fields",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nrunner = \"mock\"\ngoal = \"sneaky\"",
			wantCode: workflow.CodeAgentFields,
		},
		{
			name:     "agent with runner",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nkind = \"agent\"\ndriver = \"m\"\ngoal = \"g\"\ntools = []\nbudget_usd = 1.0\nrunner = \"mock\"",
			wantCode: workflow.CodeAgentFields,
		},
		{
			name:     "agent with fallback",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nkind = \"agent\"\ndriver = \"m\"\ngoal = \"g\"\ntools = []\nbudget_usd = 1.0\nfallback = [\"mock\"]",
			wantCode: workflow.CodeAgentFields,
		},
		{
			name:     "unknown action key",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nrunner = \"mock\"\nmystery = true",
			wantCode: workflow.CodeInvalid,
		},
		{
			name:     "bad schedule",
			toml:     "[workflow]\nname = \"w\"\nschedule = \"@fortnightly\"\n[action.a]\nrunner = \"mock\"",
			wantCode: workflow.CodeInvalid,
		},
		{
			name:     "schedule with too many fields",
			toml:     "[workflow]\nname = \"w\"\nschedule = \"* * * * * * *\"\n[action.a]\nrunner = \"mock\"",
			wantCode: workflow.CodeInvalid,
		},
		{
			name:     "task missing runner",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nidempotent = true",
			wantCode: workflow.CodeInvalid,
		},
		{
			name:     "budget bad period",
			toml:     "[workflow]\nname = \"w\"\n[workflow.budget]\nperiod = \"year\"\nlimit_usd = 5.0\non_exceed = \"pause\"\n[action.a]\nrunner = \"mock\"",
			wantCode: workflow.CodeBadBudget,
		},
		{
			name:     "budget missing period",
			toml:     "[workflow]\nname = \"w\"\n[workflow.budget]\nlimit_usd = 5.0\non_exceed = \"pause\"\n[action.a]\nrunner = \"mock\"",
			wantCode: workflow.CodeBadBudget,
		},
		{
			name:     "budget zero limit",
			toml:     "[workflow]\nname = \"w\"\n[workflow.budget]\nperiod = \"month\"\nlimit_usd = 0.0\non_exceed = \"warn\"\n[action.a]\nrunner = \"mock\"",
			wantCode: workflow.CodeBadBudget,
		},
		{
			name:     "budget negative limit",
			toml:     "[workflow]\nname = \"w\"\n[workflow.budget]\nperiod = \"month\"\nlimit_usd = -1.0\non_exceed = \"warn\"\n[action.a]\nrunner = \"mock\"",
			wantCode: workflow.CodeBadBudget,
		},
		{
			name:     "budget bad on_exceed",
			toml:     "[workflow]\nname = \"w\"\n[workflow.budget]\nperiod = \"day\"\nlimit_usd = 5.0\non_exceed = \"explode\"\n[action.a]\nrunner = \"mock\"",
			wantCode: workflow.CodeBadBudget,
		},
		{
			name:     "budget missing on_exceed",
			toml:     "[workflow]\nname = \"w\"\n[workflow.budget]\nperiod = \"day\"\nlimit_usd = 5.0\n[action.a]\nrunner = \"mock\"",
			wantCode: workflow.CodeBadBudget,
		},
		{
			name:     "budget unknown key",
			toml:     "[workflow]\nname = \"w\"\n[workflow.budget]\nperiod = \"day\"\nlimit_usd = 5.0\non_exceed = \"warn\"\nmystery = true\n[action.a]\nrunner = \"mock\"",
			wantCode: workflow.CodeBadBudget,
		},
		{
			name:     "error_workflow bad name",
			toml:     "[workflow]\nname = \"w\"\nerror_workflow = \"Bad-Name\"\n[action.a]\nrunner = \"mock\"",
			wantCode: workflow.CodeBadName,
		},
		{
			name:     "error_workflow references itself",
			toml:     "[workflow]\nname = \"w\"\nerror_workflow = \"w\"\n[action.a]\nrunner = \"mock\"",
			wantCode: workflow.CodeInvalid,
		},
		{
			name:     "poll missing source",
			toml:     "[workflow]\nname = \"w\"\nschedule = \"@daily\"\n[workflow.poll]\n[action.a]\nrunner = \"mock\"",
			wantCode: workflow.CodeBadPoll,
		},
		{
			name:     "poll without schedule",
			toml:     "[workflow]\nname = \"w\"\n[workflow.poll]\nsource = \"github_issues\"\n[action.a]\nrunner = \"mock\"",
			wantCode: workflow.CodeBadPoll,
		},
		{
			name:     "poll unknown key",
			toml:     "[workflow]\nname = \"w\"\nschedule = \"@daily\"\n[workflow.poll]\nsource = \"github_issues\"\nmystery = true\n[action.a]\nrunner = \"mock\"",
			wantCode: workflow.CodeBadPoll,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := workflow.Parse([]byte(tt.toml))
			if err == nil {
				t.Fatal("parse succeeded, want validation error")
			}

			codes := codesOf(t, err)
			if !slices.Contains(codes, tt.wantCode) {
				t.Fatalf("codes = %v, want %s present", codes, tt.wantCode)
			}
		})
	}
}

func TestParseApprovalKind(t *testing.T) {
	t.Parallel()

	rejects := []struct {
		name     string
		toml     string
		wantCode string
	}{
		{
			name: "secret ref in approval message",
			toml: "[workflow]\nname = \"w\"\n[action.a]\nkind = \"approval\"\n" +
				"message = \"approve with ${{ secrets.api_key }}?\"",
			wantCode: workflow.CodeSecretNotAllowed,
		},
		{
			name:     "approval action with runner",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nkind = \"approval\"\nrunner = \"mock\"",
			wantCode: workflow.CodeInvalid,
		},
		{
			name: "approval message references undeclared dependency",
			toml: "[workflow]\nname = \"w\"\n[action.up]\nrunner = \"mock\"\n" +
				"[action.a]\nkind = \"approval\"\nmessage = \"approve ${{ up.output.count }}?\"",
			wantCode: workflow.CodeBadReference,
		},
		{
			name:     "message on a task action",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nrunner = \"mock\"\nmessage = \"hi\"",
			wantCode: workflow.CodeInvalid,
		},
	}

	for _, tt := range rejects {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := workflow.Parse([]byte(tt.toml))
			if err == nil {
				t.Fatal("parse succeeded, want validation error")
			}

			if codes := codesOf(t, err); !slices.Contains(codes, tt.wantCode) {
				t.Fatalf("codes = %v, want %s present", codes, tt.wantCode)
			}
		})
	}

	// A valid approval action needs no fields of its own; the message is
	// optional and may read a declared upstream output.
	good := "[workflow]\nname = \"w\"\n[action.draft]\nrunner = \"mock\"\n" +
		"[action.signoff]\nkind = \"approval\"\ndepends_on = [\"draft\"]\n" +
		"message = \"approve ${{ draft.output.count }} emails?\""

	flow, err := workflow.Parse([]byte(good))
	if err != nil {
		t.Fatalf("valid approval workflow rejected: %v", err)
	}

	if flow.Actions["signoff"].Kind != workflow.KindApproval {
		t.Errorf("signoff kind = %q, want %q", flow.Actions["signoff"].Kind, workflow.KindApproval)
	}

	if flow.Actions["signoff"].Message == "" {
		t.Error("approval message not captured")
	}
}

func TestParseAccumulatesEveryError(t *testing.T) {
	t.Parallel()

	// One file, three independent problems: bad name, unknown dependency,
	// and a malformed foreach. Authors should see all of them in one pass.
	bad := "[workflow]\nname = \"Bad\"\n" +
		"[action.a]\nrunner = \"mock\"\ndepends_on = [\"ghost\"]\nforeach = \"x\""

	_, err := workflow.Parse([]byte(bad))
	if err == nil {
		t.Fatal("parse succeeded, want errors")
	}

	codes := codesOf(t, err)

	for _, want := range []string{workflow.CodeBadName, workflow.CodeUnknownDependency, workflow.CodeBadForeach} {
		if !slices.Contains(codes, want) {
			t.Errorf("codes = %v, missing %s; one pass must report every problem, not stop at the first", codes, want)
		}
	}
}

// TestParseRejectsBadRetry covers the retry table's numeric rules one field at
// a time. Each of these used to parse clean and change what the engine did at
// run time: no retries, a backwards wait, or a cap that swallowed the base.
func TestParseRejectsBadRetry(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		retry    string
		wantCode string
	}{
		{
			name:     "max_attempts below one",
			retry:    "max_attempts = 0\nbase_delay = \"1s\"",
			wantCode: workflow.CodeInvalid,
		},
		{
			name:     "max_attempts negative",
			retry:    "max_attempts = -2\nbase_delay = \"1s\"",
			wantCode: workflow.CodeInvalid,
		},
		{
			name:     "base_delay negative",
			retry:    "max_attempts = 3\nbase_delay = \"-1s\"",
			wantCode: workflow.CodeInvalid,
		},
		{
			name:     "max_delay negative",
			retry:    "max_attempts = 3\nbase_delay = \"1s\"\nmax_delay = \"-30s\"",
			wantCode: workflow.CodeInvalid,
		},
		{
			name:     "max_delay below base_delay",
			retry:    "max_attempts = 3\nbase_delay = \"30s\"\nmax_delay = \"1s\"",
			wantCode: workflow.CodeInvalid,
		},
		{
			name:     "base_delay is not a duration",
			retry:    "max_attempts = 3\nbase_delay = \"soon\"",
			wantCode: workflow.CodeBadDuration,
		},
		{
			name:     "max_delay is not a duration",
			retry:    "max_attempts = 3\nbase_delay = \"1s\"\nmax_delay = \"later\"",
			wantCode: workflow.CodeBadDuration,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := workflow.Parse([]byte(retryAction(tt.retry)))
			if err == nil {
				t.Fatal("parse succeeded, want a retry error")
			}

			if codes := codesOf(t, err); !slices.Contains(codes, tt.wantCode) {
				t.Fatalf("codes = %v, want %s present", codes, tt.wantCode)
			}
		})
	}
}

// TestParseAcceptsWellFormedRetry pins the boundary the rules above sit on: an
// uncapped policy leaves max_delay unset, and one attempt is a legal (if
// pointless) policy, so neither may be swept up by the numeric checks.
func TestParseAcceptsWellFormedRetry(t *testing.T) {
	t.Parallel()

	for _, retry := range []string{
		"max_attempts = 1\nbase_delay = \"1s\"",
		"max_attempts = 3\nbase_delay = \"1s\"",
		"max_attempts = 3\nbase_delay = \"1s\"\nmax_delay = \"30s\"",
		"max_attempts = 3\nbase_delay = \"30s\"\nmax_delay = \"30s\"",
	} {
		flow, err := workflow.Parse([]byte(retryAction(retry)))
		if err != nil {
			t.Fatalf("retry %q rejected: %v", retry, err)
		}

		if flow.Actions["a"].Retry == nil {
			t.Fatalf("retry %q parsed but the action carries no policy", retry)
		}
	}
}

// TestParseRejectsBadInputAndAgentNumbers covers two fields whose bad values
// change behavior quietly: an input type nothing can coerce, and an agent
// iteration ceiling below zero.
func TestParseRejectsBadInputAndAgentNumbers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		toml     string
		wantCode string
	}{
		{
			name:     "unknown input type",
			toml:     "[workflow]\nname = \"w\"\n[workflow.inputs.n]\ntype = \"decimal\"\n[action.a]\nrunner = \"mock\"",
			wantCode: workflow.CodeInvalid,
		},
		{
			name:     "negative max_iterations",
			toml:     agentAction("max_iterations = -1"),
			wantCode: workflow.CodeAgentFields,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := workflow.Parse([]byte(tt.toml))
			if err == nil {
				t.Fatal("parse succeeded, want a validation error")
			}

			if codes := codesOf(t, err); !slices.Contains(codes, tt.wantCode) {
				t.Fatalf("codes = %v, want %s present", codes, tt.wantCode)
			}
		})
	}
}

func TestParseRejectsCycleWithPathDetail(t *testing.T) {
	t.Parallel()

	cyclic := "[workflow]\nname = \"w\"\n" +
		"[action.a]\nrunner = \"mock\"\ndepends_on = [\"c\"]\n" +
		"[action.b]\nrunner = \"mock\"\ndepends_on = [\"a\"]\n" +
		"[action.c]\nrunner = \"mock\"\ndepends_on = [\"b\"]"

	_, err := workflow.Parse([]byte(cyclic))
	if err == nil {
		t.Fatal("parse succeeded, want cycle error")
	}

	list, ok := errors.AsType[cairnerr.Errors](err)
	if !ok {
		t.Fatalf("error is %T", err)
	}

	for _, item := range list {
		if item.Code != workflow.CodeCycleDetected {
			continue
		}

		if item.Details["cycle"] == nil {
			t.Fatal("cycle error carries no path detail")
		}

		return
	}

	t.Fatalf("no %s in %v", workflow.CodeCycleDetected, list)
}
