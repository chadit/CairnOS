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
	CodeBadForeach        = "workflow.bad_foreach"
	CodeBadPoll           = "workflow.bad_poll"
	// CodeSecretNotAllowed marks a ${{ secrets.* }} reference in a place the
	// value would be persisted: a workflow-call action's inputs (which become
	// the child run's stored inputs) or, from parity slice 5, an approval
	// message. Both round-trip through the store, so a secret there would leak
	// (.agents/spec/parity/workflow-language.md).
	CodeSecretNotAllowed = "workflow.secret_not_allowed" // #nosec G101 -- canonical error code, not a credential
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

	// KindWorkflow runs another workflow as a child action, resolving it by
	// name and passing an interpolated inputs table
	// (.agents/spec/parity/workflow-language.md, "Workflow call").
	KindWorkflow = "workflow"
	// KindApproval names the human-approval kind a later parity slice adds.
	// Reserved here by name so a file using it ahead of that slice fails with a
	// clear "reserved" message rather than a bare "unknown kind", and so
	// foreach rejects it.
	KindApproval = "approval"
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
	// ErrorWorkflow names a workflow fired once, synchronously, when a run of
	// this workflow fails. Empty means no error handler. It must match the
	// workflow-name pattern and cannot point back at this workflow
	// (.agents/spec/parity/workflow-language.md, "Error workflow").
	ErrorWorkflow string
	Notify        []string
	Timeout       time.Duration
	Inputs        map[string]InputDecl
	Actions       map[string]*Action
	// Budget is the optional per-workflow cost ceiling
	// (.agents/spec/12-ui-and-governance.md, "Budgets"). Nil means no budget,
	// so the engine never accumulates spend or refuses a run for this workflow.
	Budget *BudgetSpec
	// Poll is the optional [workflow.poll] table that turns this scheduled
	// workflow into a poll trigger: each cron fire polls an app event source
	// and enqueues one run per new event. Nil means an ordinary schedule
	// (.agents/spec/parity/triggers-webhooks.md, "Poll triggers").
	Poll *PollSpec
}

// PollSpec is one workflow's [workflow.poll] table: which app event source to
// poll on the workflow's schedule and the source-specific params that scope the
// poll. A poll workflow is an ordinary scheduled workflow, so it needs a
// non-empty top-level schedule; the fire path in internal/cli wraps the
// scheduler runner and turns one cron fire into zero or more enqueued events
// (.agents/spec/parity/triggers-webhooks.md, "Poll triggers").
type PollSpec struct {
	// Source names the registered poll source (github_issues is the first).
	// An unknown source fails at fire time with poll.ErrUnknownSource, not at
	// load, because the source registry is runtime state, not file state.
	Source string
	// Params are the source-specific arguments (repo, state, ...) passed
	// through to the source untouched.
	Params map[string]any
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

	// Foreach fans a task action over a list, running one attempt per element
	// with ${{ item }} and ${{ index }} bound. Nil means the action runs once.
	// Only legal on a task action (.agents/spec/parity/workflow-language.md).
	Foreach *ForeachSpec

	// Task-action fields.
	Runner string
	Params map[string]any
	// Fallback is the per-action provider override (D12 mixed-model). When set,
	// it replaces the engine's [engine.fallback] default for this action's
	// runner: the action's own runner runs first, then these providers in order
	// only when it fails terminally. Empty means "use the engine default".
	Fallback []string

	// Workflow-call fields (kind = "workflow"). Workflow names the child
	// workflow to run; Inputs is the interpolatable table passed as the child
	// run's inputs. Secret refs are banned in Inputs at validate time because
	// the resolved values become the child run's persisted inputs
	// (.agents/spec/parity/workflow-language.md, "Workflow call").
	Workflow string
	Inputs   map[string]any

	// Message is the optional, interpolated prompt an approval action
	// (kind = "approval") shows the operator. It can reference upstream outputs
	// but never a secret: validation bans secret refs because the resolved
	// message is persisted to the approval record
	// (.agents/spec/parity/workflow-language.md, "Human approval").
	Message string

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

// ForeachSpec is one action's [action.<name>.foreach] table: the list to fan
// over and how many elements may run at once.
type ForeachSpec struct {
	// Items is the list to iterate. It holds either a string carrying exactly
	// one whole ${{ }} reference (resolved to a list at launch) or a literal
	// array from the file.
	Items any
	// MaxParallel bounds how many elements run concurrently within the action;
	// it defaults to 1 and must be >= 1.
	MaxParallel int
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

	flow.Hash = HashBytes(data)

	return flow, nil
}

// HashBytes returns the hex SHA-256 of definition bytes, the same identity
// Load stamps onto a workflow's Hash. Inline runs share it so a run started
// from unsaved bytes records the same hash a saved file would
// (.agents/spec/parity/builder-ops.md, "Inline run").
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
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
