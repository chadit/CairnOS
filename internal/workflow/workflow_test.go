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

func TestParseAcceptsBudget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		toml       string
		wantPeriod string
		wantLimit  float64
		wantExceed string
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
			name:     "foreach reserved",
			toml:     "[workflow]\nname = \"w\"\n[action.a]\nrunner = \"mock\"\nforeach = \"items\"",
			wantCode: workflow.CodeReserved,
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

func TestParseAccumulatesEveryError(t *testing.T) {
	t.Parallel()

	// One file, three independent problems: bad name, unknown dependency,
	// and a reserved key. Authors should see all of them in one pass.
	bad := "[workflow]\nname = \"Bad\"\n" +
		"[action.a]\nrunner = \"mock\"\ndepends_on = [\"ghost\"]\nforeach = \"x\""

	_, err := workflow.Parse([]byte(bad))
	if err == nil {
		t.Fatal("parse succeeded, want errors")
	}

	codes := codesOf(t, err)

	const wantAtLeast = 3
	if len(codes) < wantAtLeast {
		t.Fatalf("got %d errors (%v), want at least %d", len(codes), codes, wantAtLeast)
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
