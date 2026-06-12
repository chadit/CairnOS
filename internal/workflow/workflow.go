package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"time"
)

// Canonical workflow.* error codes (.agents/spec/04-execution-engine.md).
const (
	CodeInvalid           = "workflow.invalid"
	CodeBadName           = "workflow.bad_name"
	CodeDuplicateAction   = "workflow.duplicate_action"
	CodeUnknownDependency = "workflow.unknown_dependency"
	CodeCycleDetected     = "workflow.cycle_detected"
	CodeBadReference      = "workflow.bad_reference"
	CodeUnknownInput      = "workflow.unknown_input"
	CodeBadDefault        = "workflow.bad_default"
	CodeBadCondition      = "workflow.bad_condition"
	CodeBadDuration       = "workflow.bad_duration"
	CodeReserved          = "workflow.reserved"
	CodeAgentFields       = "workflow.agent_fields"
	CodeBadBudget         = "workflow.bad_budget"
)

// Budget periods and on-exceed actions (.agents/spec/12-ui-and-governance.md,
// "Budgets").
const (
	BudgetPeriodDay     = "day"
	BudgetPeriodWeek    = "week"
	BudgetPeriodMonth   = "month"
	BudgetOnExceedPause = "pause"
	BudgetOnExceedWarn  = "warn"
)

// Action kinds and on_error policies.
const (
	KindTask        = "task"
	KindAgent       = "agent"
	OnErrorHalt     = "halt"
	OnErrorContinue = "continue"
)

// Declared input types.
const (
	TypeString      = "string"
	TypeInt         = "int"
	TypeFloat       = "float"
	TypeBool        = "bool"
	TypeStringSlice = "string[]"
)

// Workflow is one parsed and validated workflow file: a named DAG of actions
// (.agents/spec/02-workflow-schema.md).
type Workflow struct {
	Name        string
	Version     string
	Description string
	// Hash is the hex SHA-256 of the file bytes, stamped by Load so run
	// records can prove which definition they executed. Parse leaves it
	// empty because raw bytes from other sources carry no file identity.
	Hash     string
	Schedule string
	OnError  string
	Notify   []string
	Timeout  time.Duration
	Inputs   map[string]InputDecl
	Actions  map[string]*Action
	// Budget is the optional per-workflow cost ceiling
	// (.agents/spec/12-ui-and-governance.md, "Budgets"). Nil means no budget,
	// so the engine never accumulates spend or refuses a run for this workflow.
	Budget *BudgetSpec
}

// BudgetSpec is one workflow's [workflow.budget] table: a cost ceiling over a
// period and what to do when spend passes it. Spend rides on the estimated
// cost runners report, so the ceiling is soft and can drift from a real
// invoice (.agents/spec/12-ui-and-governance.md, "Budgets").
type BudgetSpec struct {
	// Period is the accumulation window: day, week, or month.
	Period string
	// LimitUSD is the spend ceiling for one period, in US dollars; must be > 0.
	LimitUSD float64
	// OnExceed is pause (block new runs until the period resets or an admin
	// lifts it) or warn (let runs continue but fire a notification and metric).
	OnExceed string
}

// Action is one unit of work. Kind selects between a task action (one runner
// with fixed params) and an agent action (a bounded agent loop).
type Action struct {
	Name       string
	Kind       string
	DependsOn  []string
	Idempotent bool
	Timeout    time.Duration
	Condition  string
	CondExpr   Expr
	Retry      *RetryPolicy

	// Task-action fields.
	Runner string
	Params map[string]any

	// Agent-action fields (.agents/spec/15-agent-loop.md).
	Driver        string
	Goal          string
	Tools         []string
	HasTools      bool
	MaxIterations int
	BudgetUSD     float64
	SystemPrompt  string
	MemoryScope   string
}

// RetryPolicy mirrors the per-action retry table; zero value means "use the
// engine default" (.agents/spec/04-execution-engine.md).
type RetryPolicy struct {
	MaxAttempts int
	Backoff     string
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Jitter      bool
}

// InputDecl is one declared run input.
type InputDecl struct {
	Type        string
	Required    bool
	Default     any
	HasDefault  bool
	Description string
}

// Load reads and parses a workflow file, stamping Hash with the file's
// SHA-256 so run records can identify the exact definition. The returned
// error is a cairnerr.Errors carrying every problem found, not just the
// first.
func Load(path string) (*Workflow, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the path is the operator's own CLI argument; reading it is the command's purpose
	if err != nil {
		return nil, fmt.Errorf("read workflow: %w", err)
	}

	flow, err := Parse(data)
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256(data)
	flow.Hash = hex.EncodeToString(sum[:])

	return flow, nil
}

// isValidName checks the identifier pattern shared by workflow and action
// names: a lowercase letter followed by lowercase letters, digits, or
// underscores.
func isValidName(name string) bool {
	if name == "" || !isLowerAlpha(name[0]) {
		return false
	}

	for idx := 1; idx < len(name); idx++ {
		char := name[idx]
		if !isLowerAlpha(char) && !isDigit(char) && char != '_' {
			return false
		}
	}

	return true
}

func isLowerAlpha(char byte) bool {
	return char >= 'a' && char <= 'z'
}
