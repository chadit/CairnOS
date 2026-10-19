package workflow

import (
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/chadit/CairnOS/internal/cairnerr"
)

// Parse is ParseInDir with no directory, for workflow bytes that stand alone,
// as test fixtures do. Load and ParseInDir are the entry points for workflows
// read from disk or the API, because a stdin_file param fails here: bytes with
// no file have no directory to resolve it against.
func Parse(data []byte) (*Workflow, error) {
	return ParseInDir(data, "")
}

// ParseInDir decodes one workflow and validates it against every rule in
// .agents/spec/02-workflow-schema.md, resolving each stdin_file param against
// dir, the directory the workflow file lives in. The returned error is a
// cairnerr.Errors listing all problems found, so authors fix a file in one pass.
func ParseInDir(data []byte, dir string) (*Workflow, error) {
	var raw map[string]any

	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, cairnerr.Errors{cairnerr.Newf(CodeInvalid, "not valid TOML: %v", err)}
	}

	bld := &builder{dir: dir}

	flow := bld.build(raw)
	if flow != nil {
		bld.resolveIncludes(flow)
		bld.validate(flow)
	}

	if len(bld.errs) > 0 {
		return nil, bld.errs
	}

	return flow, nil
}

// builder accumulates mapping and validation errors across the whole file.
type builder struct {
	// dir is where stdin_file paths resolve; empty refuses them.
	dir  string
	errs cairnerr.Errors
}

func (b *builder) addf(code, format string, args ...any) {
	b.errs = append(b.errs, cairnerr.Newf(code, format, args...))
}

func (b *builder) add(err *cairnerr.Error) {
	b.errs = append(b.errs, err)
}

const (
	keyWorkflow = "workflow"
	keyAction   = "action"

	// Keys that more than one table accepts.
	keyDescription = "description"
	keyTimeout     = "timeout"
	keyParams      = "params"
	keyBudget      = "budget"
)

// Retry backoff shapes, spelled the way the reliability package reads them
// when the engine hands it the policy.
const (
	backoffExponential = "exponential"
	backoffConstant    = "constant"
)

func (b *builder) build(raw map[string]any) *Workflow {
	for key := range raw {
		if key != keyWorkflow && key != keyAction {
			b.addf(CodeInvalid, "unknown top-level table %q", key)
		}
	}

	metaRaw, ok := raw[keyWorkflow].(map[string]any)
	if !ok {
		b.addf(CodeInvalid, "missing [workflow] table")

		return nil
	}

	flow := &Workflow{
		Version: "1",
		OnError: OnErrorHalt,
		Inputs:  map[string]InputDecl{},
		Actions: map[string]*Action{},
	}

	b.buildMeta(flow, metaRaw)
	b.buildActions(flow, raw[keyAction])

	return flow
}

func (b *builder) buildMeta(flow *Workflow, metaRaw map[string]any) {
	for key, value := range metaRaw {
		switch key {
		case "name":
			flow.Name = b.str("workflow.name", value)
		case "version":
			flow.Version = b.str("workflow.version", value)
		case keyDescription:
			flow.Description = b.str("workflow.description", value)
		case "schedule":
			flow.Schedule = b.str("workflow.schedule", value)
		case "on_error":
			b.buildOnError(flow, value)
		case "error_workflow":
			flow.ErrorWorkflow = b.str("workflow.error_workflow", value)
		case "notify":
			flow.Notify = b.strSlice("workflow.notify", value)
		case keyTimeout:
			flow.Timeout = b.duration("workflow.timeout", value)
		case nsInputs:
			b.buildInputs(flow, value)
		case keyBudget:
			b.buildBudget(flow, value)
		case "poll":
			b.buildPoll(flow, value)
		case "requires":
			b.buildRequires(flow, value)
		case "imports":
			b.addf(CodeReserved, "workflow key %q is reserved for a future phase", key)
		default:
			b.addf(CodeInvalid, "unknown workflow key %q", key)
		}
	}
}

func (b *builder) buildOnError(flow *Workflow, value any) {
	policy := b.str("workflow.on_error", value)

	switch policy {
	case OnErrorHalt, OnErrorContinue, "":
		if policy != "" {
			flow.OnError = policy
		}
	case "rollback":
		b.addf(CodeReserved, "on_error = %q is reserved for a future phase", policy)
	default:
		b.addf(CodeInvalid, "on_error must be %q or %q, got %q", OnErrorHalt, OnErrorContinue, policy)
	}
}

func (b *builder) buildInputs(flow *Workflow, value any) {
	table, ok := value.(map[string]any)
	if !ok {
		b.addf(CodeInvalid, "workflow.inputs must be a table")

		return
	}

	for name, declRaw := range table {
		declTable, isTable := declRaw.(map[string]any)
		if !isTable {
			b.addf(CodeInvalid, "input %q must be a table", name)

			continue
		}

		flow.Inputs[name] = b.buildInputDecl(name, declTable)
	}
}

func (b *builder) buildInputDecl(name string, declTable map[string]any) InputDecl {
	decl := InputDecl{Type: TypeString}

	for key, value := range declTable {
		switch key {
		case "type":
			decl.Type = b.str("input "+name+" type", value)
		case "required":
			decl.Required = b.boolean("input "+name+" required", value)
		case "default":
			decl.Default = value
			decl.HasDefault = true
		case keyDescription:
			decl.Description = b.str("input "+name+" description", value)
		default:
			b.addf(CodeInvalid, "input %q has unknown key %q", name, key)
		}
	}

	return decl
}

// buildBudget maps the [workflow.budget] table. Field-level checks
// (period set, limit_usd > 0, on_exceed set) run in validate so they join
// the one-pass error list; this only enforces value shapes the typed
// accessors cannot, the period and on_exceed enums.
func (b *builder) buildBudget(flow *Workflow, value any) {
	table, ok := value.(map[string]any)
	if !ok {
		b.addf(CodeBadBudget, "workflow.budget must be a table")

		return
	}

	budget := &BudgetSpec{}

	for key, fieldValue := range table {
		switch key {
		case "period":
			b.buildBudgetPeriod(budget, fieldValue)
		case "limit_usd":
			budget.LimitUSD = b.number("workflow.budget.limit_usd", fieldValue)
		case "on_exceed":
			b.buildBudgetOnExceed(budget, fieldValue)
		default:
			b.addf(CodeBadBudget, "workflow.budget has unknown key %q", key)
		}
	}

	flow.Budget = budget
}

// buildBudgetPeriod parses and validates the period enum.
func (b *builder) buildBudgetPeriod(budget *BudgetSpec, value any) {
	period := b.str("workflow.budget.period", value)

	switch period {
	case BudgetPeriodDay, BudgetPeriodWeek, BudgetPeriodMonth:
		budget.Period = period
	case "":
	default:
		b.addf(CodeBadBudget, "workflow.budget.period must be %q, %q, or %q, got %q",
			BudgetPeriodDay, BudgetPeriodWeek, BudgetPeriodMonth, period)
	}
}

// buildBudgetOnExceed parses and validates the on_exceed enum.
func (b *builder) buildBudgetOnExceed(budget *BudgetSpec, value any) {
	onExceed := b.str("workflow.budget.on_exceed", value)

	switch onExceed {
	case BudgetOnExceedPause, BudgetOnExceedWarn:
		budget.OnExceed = onExceed
	case "":
	default:
		b.addf(CodeBadBudget, "workflow.budget.on_exceed must be %q or %q, got %q",
			BudgetOnExceedPause, BudgetOnExceedWarn, onExceed)
	}
}

// buildPoll maps the [workflow.poll] table. Params pass through untouched so a
// source reads its own arguments; the source name and the schedule requirement
// are checked in validate so they join the one-pass error list. Unknown keys
// are rejected here, the house parser style.
func (b *builder) buildPoll(flow *Workflow, value any) {
	table, ok := value.(map[string]any)
	if !ok {
		b.addf(CodeBadPoll, "workflow.poll must be a table")

		return
	}

	poll := &PollSpec{}

	for key, fieldValue := range table {
		switch key {
		case "source":
			poll.Source = b.str("workflow.poll.source", fieldValue)
		case keyParams:
			poll.Params = b.table("workflow.poll.params", fieldValue)
		default:
			b.addf(CodeBadPoll, "workflow.poll has unknown key %q", key)
		}
	}

	flow.Poll = poll
}

// buildRequires maps the [workflow.requires] table. URL shape is checked in
// validate so it joins the one-pass error list; unknown keys are rejected
// here, the house parser style.
func (b *builder) buildRequires(flow *Workflow, value any) {
	table, ok := value.(map[string]any)
	if !ok {
		b.addf(CodeBadRequires, "workflow.requires must be a table")

		return
	}

	requires := &RequiresSpec{}

	for key, fieldValue := range table {
		switch key {
		case "urls":
			requires.URLs = b.strSlice("workflow.requires.urls", fieldValue)
		case "vpn":
			requires.VPN = b.str("workflow.requires.vpn", fieldValue)
		default:
			b.addf(CodeBadRequires, "workflow.requires has unknown key %q", key)
		}
	}

	flow.Requires = requires
}

func (b *builder) buildActions(flow *Workflow, actionsRaw any) {
	if actionsRaw == nil {
		return
	}

	table, ok := actionsRaw.(map[string]any)
	if !ok {
		b.addf(CodeInvalid, "[action] must be a table of tables")

		return
	}

	for name, actionRaw := range table {
		actionTable, isTable := actionRaw.(map[string]any)
		if !isTable {
			b.addf(CodeInvalid, "action %q must be a table", name)

			continue
		}

		flow.Actions[name] = b.buildAction(name, actionTable)
	}
}

func (b *builder) buildAction(name string, actionTable map[string]any) *Action {
	action := &Action{Name: name, Kind: KindTask}

	for key, value := range actionTable {
		switch key {
		case "kind":
			b.buildKind(action, value)
		case "depends_on":
			action.DependsOn = b.strSlice(name+".depends_on", value)
		case "idempotent":
			action.Idempotent = b.boolean(name+".idempotent", value)
		case keyTimeout:
			action.Timeout = b.duration(name+".timeout", value)
		case "condition":
			action.Condition = b.str(name+".condition", value)
		case "retry":
			action.Retry = b.buildRetry(name, value)
		case "runner":
			action.Runner = b.str(name+".runner", value)
		case keyParams:
			action.Params = b.table(name+".params", value)
		case "fallback":
			action.Fallback = b.strSlice(name+".fallback", value)
		case "workflow":
			action.Workflow = b.str(name+".workflow", value)
		case nsInputs:
			action.Inputs = b.table(name+".inputs", value)
		case "message":
			action.Message = b.str(name+".message", value)
		case "foreach":
			action.Foreach = b.buildForeach(name, value)
		case keyBudget:
			b.addf(CodeReserved, "action %q key %q is reserved for a future phase", name, key)
		default:
			if !b.buildAgentKey(action, name, key, value) {
				b.addf(CodeInvalid, "action %q has unknown key %q", name, key)
			}
		}
	}

	return action
}

// buildAgentKey maps the agent-action keys, reporting whether it handled key.
// Split from buildAction so the per-key switch stays under the complexity
// ceiling now that task and workflow-call keys share the same table.
func (b *builder) buildAgentKey(action *Action, name, key string, value any) bool {
	switch key {
	case "driver":
		action.Driver = b.str(name+".driver", value)
	case "goal":
		action.Goal = b.str(name+".goal", value)
	case "tools":
		action.Tools = b.strSlice(name+".tools", value)
		action.HasTools = true
	case "max_iterations":
		action.MaxIterations = b.integer(name+".max_iterations", value)
	case "budget_usd":
		action.BudgetUSD = b.number(name+".budget_usd", value)
	case "system_prompt":
		action.SystemPrompt = b.str(name+".system_prompt", value)
	case "memory_scope":
		action.MemoryScope = b.str(name+".memory_scope", value)
	default:
		return false
	}

	return true
}

func (b *builder) buildKind(action *Action, value any) {
	kind := b.str(action.Name+".kind", value)

	switch kind {
	case KindTask, KindAgent, KindWorkflow, KindApproval, "":
		if kind != "" {
			action.Kind = kind
		}
	default:
		b.addf(CodeInvalid, "action %q kind must be %q, %q, %q, or %q, got %q",
			action.Name, KindTask, KindAgent, KindWorkflow, KindApproval, kind)
	}
}

// buildForeach maps the [action.<name>.foreach] table. It captures the items
// source untyped (a string ref or a literal array) and the parallelism, both
// checked in validate; max_parallel defaults to one so an absent key runs the
// elements serially.
func (b *builder) buildForeach(name string, value any) *ForeachSpec {
	table, ok := value.(map[string]any)
	if !ok {
		b.addf(CodeBadForeach, "action %q foreach must be a table, got %T", name, value)

		return nil
	}

	spec := &ForeachSpec{MaxParallel: 1}

	for key, fieldValue := range table {
		switch key {
		case "items":
			spec.Items = fieldValue
		case "max_parallel":
			spec.MaxParallel = b.integer(name+".foreach.max_parallel", fieldValue)
		default:
			b.addf(CodeBadForeach, "action %q foreach has unknown key %q", name, key)
		}
	}

	return spec
}

func (b *builder) buildRetry(name string, value any) *RetryPolicy {
	table, ok := value.(map[string]any)
	if !ok {
		b.addf(CodeInvalid, "action %q retry must be a table", name)

		return nil
	}

	// Jitter defaults on: retry storms from synchronized backoff are exactly
	// what the spec's full-jitter requirement exists to prevent.
	policy := &RetryPolicy{Backoff: backoffExponential, Jitter: true}

	for key, fieldValue := range table {
		switch key {
		case "max_attempts":
			policy.MaxAttempts = b.integer(name+".retry.max_attempts", fieldValue)
		case "backoff":
			b.buildBackoff(name, policy, fieldValue)
		case "base_delay":
			policy.BaseDelay = b.duration(name+".retry.base_delay", fieldValue)
		case "max_delay":
			policy.MaxDelay = b.duration(name+".retry.max_delay", fieldValue)
		case "jitter":
			policy.Jitter = b.boolean(name+".retry.jitter", fieldValue)
		default:
			b.addf(CodeInvalid, "action %q retry has unknown key %q", name, key)
		}
	}

	return policy
}

func (b *builder) buildBackoff(name string, policy *RetryPolicy, value any) {
	backoff := b.str(name+".retry.backoff", value)

	switch backoff {
	case backoffExponential, backoffConstant, "":
		if backoff != "" {
			policy.Backoff = backoff
		}
	default:
		b.addf(CodeInvalid, "action %q retry.backoff must be exponential or constant, got %q", name, backoff)
	}
}

// Typed accessors. Each reports a CodeInvalid error and returns the zero
// value on a type mismatch, so mapping continues and later problems still
// surface in the same pass.

func (b *builder) str(field string, value any) string {
	text, ok := value.(string)
	if !ok {
		b.addf(CodeInvalid, "%s must be a string, got %T", field, value)

		return ""
	}

	return text
}

func (b *builder) boolean(field string, value any) bool {
	flag, ok := value.(bool)
	if !ok {
		b.addf(CodeInvalid, "%s must be a bool, got %T", field, value)

		return false
	}

	return flag
}

func (b *builder) integer(field string, value any) int {
	num, ok := value.(int64)
	if !ok {
		b.addf(CodeInvalid, "%s must be an integer, got %T", field, value)

		return 0
	}

	return int(num)
}

func (b *builder) number(field string, value any) float64 {
	switch num := value.(type) {
	case float64:
		return num
	case int64:
		return float64(num)
	default:
		b.addf(CodeInvalid, "%s must be a number, got %T", field, value)

		return 0
	}
}

func (b *builder) strSlice(field string, value any) []string {
	items, ok := value.([]any)
	if !ok {
		b.addf(CodeInvalid, "%s must be an array of strings, got %T", field, value)

		return nil
	}

	out := make([]string, 0, len(items))

	for _, item := range items {
		text, isStr := item.(string)
		if !isStr {
			b.addf(CodeInvalid, "%s must contain only strings, found %T", field, item)

			continue
		}

		out = append(out, text)
	}

	return out
}

func (b *builder) table(field string, value any) map[string]any {
	table, ok := value.(map[string]any)
	if !ok {
		b.addf(CodeInvalid, "%s must be a table, got %T", field, value)

		return nil
	}

	return table
}

func (b *builder) duration(field string, value any) time.Duration {
	text, ok := value.(string)
	if !ok {
		b.addf(CodeBadDuration, "%s must be a duration string, got %T", field, value)

		return 0
	}

	parsed, err := ParseDuration(text)
	if err != nil {
		b.addf(CodeBadDuration, "%s: %v", field, err)

		return 0
	}

	return parsed
}
