package workflow

import (
	"crypto/sha1" // #nosec G505 -- git names objects by SHA-1; matching its IDs secures nothing on its own
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/chadit/CairnOS/pkg/runner"
)

// Canonical workflow.* error codes.
const (
	CodeInvalid           = "workflow.invalid"
	CodeBadName           = "workflow.bad_name"
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
	CodeBadRequires       = "workflow.bad_requires"
	// CodeSecretNotAllowed marks a ${{ secrets.* }} reference in a place the
	// value would be persisted: a workflow-call action's inputs (which become
	// the child run's stored inputs) or an approval message. Both round-trip
	// through the store, so a secret there would leak.
	CodeSecretNotAllowed = "workflow.secret_not_allowed" // #nosec G101 -- canonical error code, not a credential
)

// Budget periods and on-exceed actions.
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
	// name and passing an interpolated inputs table.
	KindWorkflow = "workflow"
	// KindApproval holds the run at this action until a person approves or
	// denies it.
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

// Workflow is one parsed and validated workflow file: a named DAG of actions.
type Workflow struct {
	Inputs map[string]InputDecl

	// Requires is the optional [workflow.requires] table naming what must be
	// reachable before a scheduled fire is worth running. Nil means the
	// workflow fires regardless.
	Requires *RequiresSpec

	// Poll is the optional [workflow.poll] table that turns this scheduled
	// workflow into a poll trigger: each cron fire polls an app event source
	// and enqueues one run per new event. Nil means an ordinary schedule.
	Poll *PollSpec

	// Budget is the optional per-workflow cost ceiling. Nil means no budget, so
	// the engine never accumulates spend or refuses a run for this workflow.
	Budget *BudgetSpec

	Actions map[string]*Action

	// includes are the stdin_file texts read at load, which DefinitionHash
	// folds into the identity alongside the file bytes.
	includes []includedFile

	// Hash is the hex SHA-256 of the file bytes, stamped by Load so run
	// records can prove which definition they executed. Parse leaves it
	// empty because raw bytes from other sources carry no file identity.
	Hash string

	// ErrorWorkflow names a workflow fired once, synchronously, when a run of
	// this workflow fails. Empty means no error handler. It must match the
	// workflow-name pattern and cannot point back at this workflow.
	ErrorWorkflow string

	OnError     string
	Schedule    string
	Name        string
	Description string
	Version     string
	Notify      []string
	Timeout     time.Duration
}

// RequiresSpec is one workflow's [workflow.requires] table. The fire path in
// internal/cli probes it every minute: while a URL cannot be reached or the VPN
// is down, fires are skipped and an in-flight run is held rather than
// dead-lettered. The probe answers once what every run would otherwise answer
// with a traceback.
type RequiresSpec struct {
	// VPN names a network interface (tun0, wg0) the URLs are reached through.
	// Empty means none. When it is down, the notification says so instead of
	// reporting the dial failure it causes.
	VPN string
	// URLs are probed by a TCP dial to each host and port, defaulting to 443
	// on https and 80 on http. The full URL is kept so a notification names
	// the repository page the operator recognizes rather than a bare hostname.
	URLs []string
}

// PollSpec is one workflow's [workflow.poll] table: which app event source to
// poll and the source-specific params that scope the poll. A poll workflow is
// an ordinary scheduled workflow, so it needs a non-empty top-level schedule;
// the fire path in internal/cli turns one cron fire into zero or more enqueued
// events.
type PollSpec struct {
	// Params are the source-specific arguments (repo, state, ...) passed
	// through to the source untouched.
	Params map[string]any

	// Source names the registered poll source (github_issues is the first).
	// An unknown source fails at fire time with poll.ErrUnknownSource, not at
	// load, because the source registry is runtime state, not file state.
	Source string
}

// BudgetSpec is one workflow's [workflow.budget] table: a cost ceiling over a
// period and what to do when spend passes it. Spend rides on the estimated
// cost runners report, so the ceiling is soft and can drift from a real
// invoice.
type BudgetSpec struct {
	// Period is the accumulation window: day, week, or month.
	Period string

	// OnExceed is pause (block new runs until the period resets or an admin
	// lifts it) or warn (let runs continue but fire a notification and metric).
	OnExceed string

	// LimitUSD is the spend ceiling for one period, in US dollars; must be > 0.
	LimitUSD float64
}

// Action is one unit of work. Kind selects between a task action (one runner
// with fixed params) and an agent action (a bounded agent loop).
type Action struct {
	CondExpr *Expr

	// Inputs is the interpolatable table a workflow-call action passes as the
	// child run's inputs. Secret refs are banned here at validate time because
	// the resolved values become the child run's persisted inputs.
	Inputs map[string]any

	Params map[string]any

	// Foreach fans a task action over a list, running one attempt per element
	// with ${{ item }} and ${{ index }} bound. Nil means the action runs once.
	// Only legal on a task action.
	Foreach *ForeachSpec

	Retry     *RetryPolicy
	Condition string

	// Driver, MemoryScope, and SystemPrompt configure an agent action, as do
	// Goal, Tools, HasTools, MaxIterations, and BudgetUSD further down.
	Driver       string
	MemoryScope  string
	SystemPrompt string

	// Runner is the task action's runner name; Params carries its arguments.
	Runner string

	Name string

	// Goal is the objective an agent action works toward.
	Goal string

	// Workflow names the child workflow a workflow-call action (kind =
	// "workflow") runs, with Inputs above as that child run's inputs.
	Workflow string

	// Definition is the workflow file Load read this action from, then the
	// stdin_file the action included, each with the object ID of the bytes
	// read; empty for a workflow parsed from bytes. It rides on the action
	// because the engine hands runners one action at a time, and the exec gate
	// reads it to recognize files a trusted source committed.
	Definition []runner.DefinitionFile

	Kind string

	// Message is the optional, interpolated prompt an approval action
	// (kind = "approval") shows the operator. It can reference upstream outputs
	// but never a secret: validation bans secret refs because the resolved
	// message is persisted to the approval record.
	Message string

	DependsOn []string

	// Fallback is the per-action provider override (D12 mixed-model). When set,
	// it replaces the engine's [engine.fallback] default for this action's
	// runner: the action's own runner runs first, then these providers in order
	// only when it fails terminally. Empty means "use the engine default".
	Fallback []string

	// Tools, MaxIterations, BudgetUSD, and HasTools bound an agent action's
	// tool use, loop count, and spend.
	Tools         []string
	MaxIterations int
	BudgetUSD     float64

	Timeout time.Duration

	HasTools bool

	Idempotent bool
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
// engine default".
type RetryPolicy struct {
	Backoff     string
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Jitter      bool
}

// InputDecl is one declared run input.
type InputDecl struct {
	Default     any
	Type        string
	Description string
	Required    bool
	HasDefault  bool
}

// MustBeSupplied reports whether a run fails unless its caller passes this
// input: it is required and has no default to fall back on. `cairnos schedule
// add` and the engine's run-time input check both ask this, so the add-time
// refusal cannot drift from the rule that rejects the run.
func (decl InputDecl) MustBeSupplied() bool {
	return decl.Required && !decl.HasDefault
}

// Load reads and parses a workflow file, resolving stdin_file params against
// the file's directory, and stamps Hash with DefinitionHash so run records can
// identify the exact definition, plus each action's Definition. The returned
// error is a cairnerr.Errors carrying every problem found, not just the first.
func Load(path string) (*Workflow, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the path is the operator's own CLI argument; reading it is the command's purpose
	if err != nil {
		return nil, fmt.Errorf("read workflow: %w", err)
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve workflow path: %w", err)
	}

	flow, err := ParseInDir(data, filepath.Dir(path))
	if err != nil {
		return nil, err
	}

	flow.Hash = flow.DefinitionHash(data)
	flow.stampDefinitions(absPath, data)

	return flow, nil
}

// stampDefinitions records on each action the files it was built from. Each
// action gets its own slice, so appending an include to one never shows up in
// another that shares the workflow file.
func (flow *Workflow) stampDefinitions(absPath string, data []byte) {
	workflowFile := runner.DefinitionFile{Path: absPath, BlobID: GitBlobID(data)}

	for _, action := range flow.Actions {
		action.Definition = []runner.DefinitionFile{workflowFile}
	}

	dir := filepath.Dir(absPath)

	for _, file := range flow.includes {
		action := flow.Actions[file.action]
		action.Definition = append(action.Definition, runner.DefinitionFile{
			Path:   filepath.Join(dir, file.path),
			BlobID: GitBlobID([]byte(file.text)),
		})
	}
}

// GitBlobID is the object ID a SHA-1 git repository gives data stored as a
// file, computed here so the exec gate compares the bytes Load read with a
// commit instead of asking git to hash a file that may have changed since. A
// SHA-256 repository names objects differently, so its files never match and
// the gate denies.
func GitBlobID(data []byte) string {
	hasher := sha1.New() // #nosec G401 -- git's object naming, not a security primitive here
	hasher.Write([]byte("blob " + strconv.Itoa(len(data)) + "\x00"))
	hasher.Write(data)

	return hex.EncodeToString(hasher.Sum(nil))
}

// HashBytes returns the hex SHA-256 of definition bytes, the same identity
// Load stamps onto a workflow's Hash. Inline runs share it so a run started
// from unsaved bytes records the same hash a saved file would.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

// ValidName checks the identifier pattern shared by workflow and action
// names: a lowercase letter followed by lowercase letters, digits, or the '_'
// character. It is exported so a command that edits an exec allow list refuses
// a name no workflow could declare.
func ValidName(name string) bool {
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
