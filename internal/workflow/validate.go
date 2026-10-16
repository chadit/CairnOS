package workflow

import (
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/chadit/CairnOS/internal/cairnerr"
)

const defaultMaxIterations = 10

// validate runs every load-time rule from 02-workflow-schema.md against the
// built workflow. Runner existence and tool capability checks happen at run
// start instead, because the registry is runtime state, not file state.
func (b *builder) validate(flow *Workflow) {
	b.checkWorkflowName(flow)
	b.checkErrorWorkflow(flow)
	b.checkSchedule(flow)
	b.checkInputs(flow)
	b.checkBudget(flow)
	b.checkPoll(flow)
	b.checkRequires(flow)

	for _, name := range slices.Sorted(maps.Keys(flow.Actions)) {
		b.checkAction(flow, flow.Actions[name])
	}

	b.checkCycles(flow)
}

func (b *builder) checkWorkflowName(flow *Workflow) {
	if flow.Name == "" {
		b.addf(CodeBadName, "workflow.name is required")

		return
	}

	if !isValidName(flow.Name) {
		b.addf(CodeBadName, "workflow.name %q must match a lowercase identifier", flow.Name)
	}
}

// checkErrorWorkflow enforces the error_workflow rules: an empty value means no
// error handler, a set value must match the workflow-name pattern, and it
// cannot point back at the workflow that declares it, because a run triggered
// as "error" never fires its own error workflow anyway and a self-reference
// only ever signals a mistake (.agents/spec/parity/workflow-language.md,
// "Error workflow").
func (b *builder) checkErrorWorkflow(flow *Workflow) {
	if flow.ErrorWorkflow == "" {
		return
	}

	if !isValidName(flow.ErrorWorkflow) {
		b.addf(CodeBadName, "workflow.error_workflow %q must match a lowercase identifier", flow.ErrorWorkflow)

		return
	}

	if flow.ErrorWorkflow == flow.Name {
		b.addf(CodeInvalid, "workflow.error_workflow cannot reference the workflow itself")
	}
}

// A schedule is an accepted shorthand, a 5-field cron expression, or a 6-field
// one whose leading field is seconds. Field ranges and the scheduler's
// fire-rate floor are checked when the schedule is added, where the cron parser
// lives; this package stays free of that dependency.
const (
	cronFieldCount     = 5
	cronSecondsFieldCt = 6
)

func (b *builder) checkSchedule(flow *Workflow) {
	schedule := flow.Schedule
	if schedule == "" {
		return
	}

	if strings.HasPrefix(schedule, "@") {
		switch schedule {
		case "@hourly", "@daily", "@weekly", "@monthly":
			return
		default:
			b.addf(CodeInvalid, "schedule shorthand %q is not supported", schedule)

			return
		}
	}

	if fields := len(strings.Fields(schedule)); fields != cronFieldCount && fields != cronSecondsFieldCt {
		b.addf(CodeInvalid,
			"schedule %q must be a 5-field cron expression, a 6-field one led by seconds, or an @-shorthand",
			schedule)
	}
}

func (b *builder) checkInputs(flow *Workflow) {
	for _, name := range slices.Sorted(maps.Keys(flow.Inputs)) {
		decl := flow.Inputs[name]

		if !inputTypeKnown(decl.Type) {
			b.addf(CodeInvalid, "input %q type %q is not supported", name, decl.Type)

			continue
		}

		if decl.HasDefault && !inputTypeMatches(decl.Type, decl.Default) {
			b.addf(CodeBadDefault, "input %q default %v does not match type %q", name, decl.Default, decl.Type)
		}
	}
}

// checkBudget enforces the required budget fields when a [workflow.budget]
// table is present: period and on_exceed must be set (the parser already
// rejected bad enum values, so an empty value here means the key was
// omitted), and limit_usd must be positive.
func (b *builder) checkBudget(flow *Workflow) {
	if flow.Budget == nil {
		return
	}

	if flow.Budget.Period == "" {
		b.addf(CodeBadBudget, "workflow.budget.period is required (%q, %q, or %q)",
			BudgetPeriodDay, BudgetPeriodWeek, BudgetPeriodMonth)
	}

	if flow.Budget.LimitUSD <= 0 {
		b.addf(CodeBadBudget, "workflow.budget.limit_usd must be greater than zero, got %v", flow.Budget.LimitUSD)
	}

	if flow.Budget.OnExceed == "" {
		b.addf(CodeBadBudget, "workflow.budget.on_exceed is required (%q or %q)",
			BudgetOnExceedPause, BudgetOnExceedWarn)
	}
}

// checkPoll enforces the [workflow.poll] rules: source is required, and a poll
// needs a top-level schedule because a poll with no cadence never fires
// (.agents/spec/parity/triggers-webhooks.md, "Poll triggers"). Whether the
// source name resolves is a runtime concern, checked at fire time against the
// registered source map, so an unknown source is not a load error.
func (b *builder) checkPoll(flow *Workflow) {
	if flow.Poll == nil {
		return
	}

	if flow.Poll.Source == "" {
		b.addf(CodeBadPoll, "workflow.poll.source is required")
	}

	if flow.Schedule == "" {
		b.addf(CodeBadPoll, "workflow.poll needs a schedule; a poll with no cadence never fires")
	}
}

// checkRequires enforces the [workflow.requires] rules: at least one URL, each
// an absolute http or https URL with a host, since the probe dials the host
// and the notification prints the URL. Whether the host answers is the fire
// path's concern, checked every minute, so an unreachable host is not a load
// error.
func (b *builder) checkRequires(flow *Workflow) {
	if flow.Requires == nil {
		return
	}

	if len(flow.Requires.URLs) == 0 {
		b.addf(CodeBadRequires, "workflow.requires.urls needs at least one URL")
	}

	for _, raw := range flow.Requires.URLs {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			b.addf(CodeBadRequires, "workflow.requires.urls entry %q must be an absolute http or https URL", raw)
		}
	}
}

func inputTypeKnown(typeName string) bool {
	switch typeName {
	case TypeString, TypeInt, TypeFloat, TypeBool, TypeStringSlice:
		return true
	default:
		return false
	}
}

func inputTypeMatches(typeName string, value any) bool {
	switch typeName {
	case TypeString:
		_, ok := value.(string)

		return ok
	case TypeInt:
		_, ok := value.(int64)

		return ok
	case TypeFloat:
		return matchesFloat(value)
	case TypeBool:
		_, ok := value.(bool)

		return ok
	default:
		return matchesStringSlice(value)
	}
}

func matchesFloat(value any) bool {
	switch value.(type) {
	case float64, int64:
		return true
	default:
		return false
	}
}

func matchesStringSlice(value any) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}

	for _, item := range items {
		if _, isStr := item.(string); !isStr {
			return false
		}
	}

	return true
}

func (b *builder) checkAction(flow *Workflow, action *Action) {
	if !isValidName(action.Name) {
		b.addf(CodeBadName, "action name %q must match a lowercase identifier", action.Name)
	}

	for _, dep := range action.DependsOn {
		if _, exists := flow.Actions[dep]; !exists {
			b.addf(CodeUnknownDependency, "action %q depends on unknown action %q", action.Name, dep)
		}
	}

	if action.Kind == KindAgent {
		b.checkAgentAction(action)
	}

	if action.Kind == KindTask {
		b.checkTaskAction(action)
	}

	if action.Kind == KindWorkflow {
		b.checkWorkflowAction(flow, action)
	}

	if action.Kind == KindApproval {
		b.checkApprovalAction(flow, action)
	}

	if action.Kind != KindApproval && action.Message != "" {
		b.addf(CodeInvalid, "action %q sets message, which belongs to an approval action", action.Name)
	}

	b.checkCondition(action)
	b.checkRetry(action)
	b.checkForeach(flow, action)
	b.checkActionRefs(flow, action)
}

// checkRetry enforces the retry table's numeric rules. The parser only proves
// the fields have the right shape, so without these an author who asked for
// retries can get one attempt (max_attempts below one), a backoff that waits
// backwards (a negative delay), or every wait truncated to a cap below the
// stated base (max_delay under base_delay), all silently.
func (b *builder) checkRetry(action *Action) {
	if action.Retry == nil {
		return
	}

	if action.Retry.MaxAttempts < 1 {
		b.addf(CodeInvalid, "action %q retry.max_attempts must be at least 1, got %d",
			action.Name, action.Retry.MaxAttempts)
	}

	if action.Retry.BaseDelay < 0 {
		b.addf(CodeInvalid, "action %q retry.base_delay cannot be negative, got %s",
			action.Name, action.Retry.BaseDelay)
	}

	if action.Retry.MaxDelay < 0 {
		b.addf(CodeInvalid, "action %q retry.max_delay cannot be negative, got %s",
			action.Name, action.Retry.MaxDelay)
	}

	// A zero max_delay means uncapped, so the ordering rule only applies to a
	// cap the file actually set.
	if action.Retry.MaxDelay > 0 && action.Retry.MaxDelay < action.Retry.BaseDelay {
		b.addf(CodeInvalid, "action %q retry.max_delay %s is below retry.base_delay %s, so every wait would be capped at the smaller value",
			action.Name, action.Retry.MaxDelay, action.Retry.BaseDelay)
	}
}

// checkWorkflowAction enforces the workflow-call kind's rules: it names a child
// workflow by the shared identifier pattern, uses none of the task or agent
// fields, and its inputs table interpolates upstream data but never a secret,
// because those inputs become the child run's persisted inputs
// (.agents/spec/parity/workflow-language.md, "Workflow call").
func (b *builder) checkWorkflowAction(flow *Workflow, action *Action) {
	switch {
	case action.Workflow == "":
		b.addf(CodeInvalid, "workflow-call action %q is missing its workflow", action.Name)
	case !isValidName(action.Workflow):
		b.addf(CodeBadName, "workflow-call action %q workflow %q must match a lowercase identifier", action.Name, action.Workflow)
	}

	if action.Runner != "" || action.Params != nil || len(action.Fallback) > 0 {
		b.addf(CodeInvalid, "workflow-call action %q cannot use runner, params, or fallback; those belong to task actions", action.Name)
	}

	agentOnly := action.Driver != "" || action.Goal != "" || action.HasTools ||
		action.BudgetUSD != 0 || action.MaxIterations != 0 ||
		action.SystemPrompt != "" || action.MemoryScope != ""
	if agentOnly {
		b.addf(CodeAgentFields,
			"workflow-call action %q cannot use driver, goal, tools, max_iterations, budget_usd, system_prompt, or memory_scope; those are agent-only",
			action.Name)
	}

	b.checkWorkflowCallInputs(flow, action)
}

// checkWorkflowCallInputs walks the inputs table a workflow-call action passes
// to its child and applies two rules: rule 5 (an action-output ref names a
// declared dependency) and the secret ban (a ${{ secrets.* }} ref is rejected,
// since the resolved value would be written to the child's runs.inputs_json).
func (b *builder) checkWorkflowCallInputs(flow *Workflow, action *Action) {
	for _, text := range collectValueTexts(action.Inputs) {
		refs, refErrs := ExtractRefs(text)

		for _, err := range refErrs {
			b.addf(CodeBadReference, "action %q: %v", action.Name, err)
		}

		for _, ref := range refs {
			if ref.Kind == RefSecret {
				b.addf(CodeSecretNotAllowed,
					"action %q inputs read %s: secret refs are not allowed in workflow-call inputs; the child workflow resolves its own secrets",
					action.Name, ref.Raw)

				continue
			}

			b.checkRef(flow, action, ref)
		}
	}
}

// checkApprovalAction enforces the human-approval kind's rules: it uses none of
// the task, agent, or workflow-call fields, and its optional message
// interpolates upstream data but never a secret, because the resolved message is
// persisted to the approval record (.agents/spec/parity/workflow-language.md,
// "Human approval"). The foreach ban rides checkForeach, which rejects a
// non-task foreach for every kind.
func (b *builder) checkApprovalAction(flow *Workflow, action *Action) {
	if action.Runner != "" || action.Params != nil || len(action.Fallback) > 0 {
		b.addf(CodeInvalid, "approval action %q cannot use runner, params, or fallback; those belong to task actions", action.Name)
	}

	agentOnly := action.Driver != "" || action.Goal != "" || action.HasTools ||
		action.BudgetUSD != 0 || action.MaxIterations != 0 ||
		action.SystemPrompt != "" || action.MemoryScope != ""
	if agentOnly {
		b.addf(CodeAgentFields,
			"approval action %q cannot use driver, goal, tools, max_iterations, budget_usd, system_prompt, or memory_scope; those are agent-only",
			action.Name)
	}

	if action.Workflow != "" || action.Inputs != nil {
		b.addf(CodeInvalid, "approval action %q cannot use workflow or inputs; those belong to a workflow-call action", action.Name)
	}

	b.checkApprovalMessage(flow, action)
}

// checkApprovalMessage walks the approval message and applies two rules: rule 5
// (an action-output ref names a declared dependency) and the secret ban (a
// ${{ secrets.* }} ref is rejected, since the resolved message is persisted to
// run_approvals.message).
func (b *builder) checkApprovalMessage(flow *Workflow, action *Action) {
	refs, refErrs := ExtractRefs(action.Message)

	for _, err := range refErrs {
		b.addf(CodeBadReference, "action %q: %v", action.Name, err)
	}

	for _, ref := range refs {
		if ref.Kind == RefSecret {
			b.addf(CodeSecretNotAllowed,
				"action %q message reads %s: secret refs are not allowed in an approval message; it is persisted to the approval record",
				action.Name, ref.Raw)

			continue
		}

		b.checkRef(flow, action, ref)
	}
}

// checkForeach enforces the foreach rules: it only rides a task action,
// max_parallel stays at least one, and items is either one whole reference
// (bound by rule 5) or a literal array
// (.agents/spec/parity/workflow-language.md, "foreach").
func (b *builder) checkForeach(flow *Workflow, action *Action) {
	if action.Foreach == nil {
		return
	}

	if action.Kind != KindTask {
		b.addf(CodeBadForeach, "action %q is kind %q; foreach is only valid on a task action", action.Name, action.Kind)
	}

	if action.Foreach.MaxParallel < 1 {
		b.addf(CodeBadForeach, "action %q foreach max_parallel must be >= 1, got %d", action.Name, action.Foreach.MaxParallel)
	}

	b.checkForeachItems(flow, action)
}

// checkForeachItems validates the items source: a whole ${{ }} reference or a
// literal array. A reference must follow rule 5, and item/index cannot seed
// their own list.
func (b *builder) checkForeachItems(flow *Workflow, action *Action) {
	switch items := action.Foreach.Items.(type) {
	case string:
		ref, whole := WholeRef(items)
		if !whole {
			b.addf(CodeBadForeach,
				"action %q foreach items must be exactly one ${{ }} reference or a literal array", action.Name)

			return
		}

		b.checkForeachItemsRef(flow, action, ref)
	case []any:
		// A literal array is a fixed list; any references inside its elements
		// resolve at launch through the same resolver as params, so a bad ref
		// there surfaces as a precondition failure rather than a load error.
	case nil:
		b.addf(CodeBadForeach, "action %q foreach is missing its items", action.Name)
	default:
		b.addf(CodeBadForeach,
			"action %q foreach items must be a ${{ }} reference or a literal array, got %T", action.Name, items)
	}
}

// checkForeachItemsRef applies rule 5 to a whole-reference items source: an
// action-output ref must name a declared dependency, an input ref must be
// declared, and item/index are illegal here since the list precedes any
// per-element context.
func (b *builder) checkForeachItemsRef(flow *Workflow, action *Action, ref Ref) {
	switch ref.Kind {
	case RefAction:
		b.checkActionOutputRef(flow, action, ref)
	case RefInput:
		if _, declared := flow.Inputs[ref.Target]; !declared {
			b.addf(CodeUnknownInput, "action %q foreach items references undeclared input %q", action.Name, ref.Target)
		}
	case RefItem, RefIndex:
		b.addf(CodeBadForeach, "action %q foreach items cannot reference %s", action.Name, ref.Raw)
	case RefSystem:
		b.checkSystemRef(action, ref)
	case RefEnv, RefSecret:
		// Shape checked at parse time; runtime resolves the value.
	}
}

// checkTaskAction enforces rule 11's mirror image: task actions use runner
// and params, never the agent fields.
func (b *builder) checkTaskAction(action *Action) {
	if action.Runner == "" {
		b.addf(CodeInvalid, "action %q is missing its runner", action.Name)
	}

	agentOnly := action.Driver != "" || action.Goal != "" || action.HasTools ||
		action.BudgetUSD != 0 || action.MaxIterations != 0 ||
		action.SystemPrompt != "" || action.MemoryScope != ""
	if agentOnly {
		b.addf(CodeAgentFields,
			"action %q is a task action; driver, goal, tools, max_iterations, budget_usd, system_prompt, and memory_scope are agent-only",
			action.Name)
	}

	if action.Workflow != "" || action.Inputs != nil {
		b.addf(CodeInvalid, "action %q is a task action; workflow and inputs belong to a workflow-call action", action.Name)
	}
}

// checkAgentAction enforces rule 11: driver, goal, and budget_usd are
// required, tools must be declared, and the task fields are forbidden
// (.agents/spec/15-agent-loop.md).
func (b *builder) checkAgentAction(action *Action) {
	if action.Driver == "" {
		b.addf(CodeAgentFields, "agent action %q is missing its driver", action.Name)
	}

	if action.Goal == "" {
		b.addf(CodeAgentFields, "agent action %q is missing its goal", action.Name)
	}

	if action.BudgetUSD <= 0 {
		b.addf(CodeAgentFields, "agent action %q needs budget_usd > 0; there is no unlimited mode", action.Name)
	}

	if !action.HasTools {
		b.addf(CodeAgentFields, "agent action %q must declare tools (an empty list means reasoning only)", action.Name)
	}

	if action.Runner != "" || action.Params != nil || len(action.Fallback) > 0 {
		b.addf(CodeAgentFields, "agent action %q cannot use runner, params, or fallback; those belong to task actions", action.Name)
	}

	if action.Workflow != "" || action.Inputs != nil {
		b.addf(CodeAgentFields, "agent action %q cannot use workflow or inputs; those belong to a workflow-call action", action.Name)
	}

	if action.MaxIterations < 0 {
		b.addf(CodeAgentFields, "agent action %q max_iterations cannot be negative", action.Name)
	}

	if action.MaxIterations == 0 {
		action.MaxIterations = defaultMaxIterations
	}
}

func (b *builder) checkCondition(action *Action) {
	if action.Condition == "" {
		return
	}

	expr, err := ParseCondition(action.Condition)
	if err != nil {
		b.addf(CodeBadCondition, "action %q condition: %v", action.Name, err)

		return
	}

	action.CondExpr = expr
}

// checkActionRefs walks every string the action will interpolate (params,
// goal, condition) and applies rule 5: an action-output reference must point
// at a declared dependency, which keeps the data graph and the dependency
// graph in sync.
func (b *builder) checkActionRefs(flow *Workflow, action *Action) {
	texts := collectRefTexts(action)

	for _, text := range texts {
		refs, refErrs := ExtractRefs(text)

		for _, err := range refErrs {
			b.addf(CodeBadReference, "action %q: %v", action.Name, err)
		}

		for _, ref := range refs {
			b.checkRef(flow, action, ref)
		}
	}
}

func collectRefTexts(action *Action) []string {
	return slices.Concat([]string{action.Goal, action.Condition}, collectValueTexts(action.Params))
}

// collectValueTexts gathers every string reachable inside a params- or
// inputs-shaped value, recursing through lists and tables so a reference buried
// in a nested structure still reaches validation.
func collectValueTexts(value any) []string {
	var texts []string

	var walk func(value any)

	walk = func(value any) {
		switch typed := value.(type) {
		case string:
			texts = append(texts, typed)
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			for _, name := range slices.Sorted(maps.Keys(typed)) {
				walk(typed[name])
			}
		}
	}

	walk(value)

	return texts
}

func (b *builder) checkRef(flow *Workflow, action *Action, ref Ref) {
	switch ref.Kind {
	case RefAction:
		b.checkActionOutputRef(flow, action, ref)
	case RefInput:
		if _, declared := flow.Inputs[ref.Target]; !declared {
			b.addf(CodeUnknownInput, "action %q references undeclared input %q", action.Name, ref.Target)
		}
	case RefItem, RefIndex:
		// item and index only mean something inside a fan-out, so a reference
		// to either from an action with no foreach is a load-time error.
		if action.Foreach == nil {
			b.addf(CodeBadForeach, "action %q reads %s but declares no foreach", action.Name, ref.Raw)
		}
	case RefSystem:
		b.checkSystemRef(action, ref)
	case RefEnv, RefSecret:
		// Shape was checked at parse time. Allowlist and secret existence are
		// runtime config concerns, enforced when the engine resolves values.
	}
}

// checkSystemRef refuses a system.* name the engine cannot answer, so a typo
// fails at load like an undeclared input instead of at the first scheduled run.
func (b *builder) checkSystemRef(action *Action, ref Ref) {
	if ref.Target != SystemHostname {
		b.addf(CodeBadReference, "action %q references unknown system value %s; only system.%s exists",
			action.Name, ref.Raw, SystemHostname)
	}
}

func (b *builder) checkActionOutputRef(flow *Workflow, action *Action, ref Ref) {
	if _, exists := flow.Actions[ref.Target]; !exists {
		b.addf(CodeBadReference, "action %q references unknown action %q", action.Name, ref.Target)

		return
	}

	if !slices.Contains(action.DependsOn, ref.Target) {
		b.addf(CodeBadReference,
			"action %q reads %s but does not declare %q in depends_on",
			action.Name, ref.Raw, ref.Target)
	}
}

// checkCycles runs a three-color depth-first search and reports the cycle
// path by name, per rule 4's requirement that the error names the cycle.
func (b *builder) checkCycles(flow *Workflow) {
	const (
		white = 0
		gray  = 1
		black = 2
	)

	colors := map[string]int{}

	var stack []string

	var visit func(name string) []string

	visit = func(name string) []string {
		colors[name] = gray
		stack = append(stack, name)

		action := flow.Actions[name]

		for _, dep := range action.DependsOn {
			if _, exists := flow.Actions[dep]; !exists {
				continue // already reported as an unknown dependency
			}

			if colors[dep] == gray {
				return append(slices.Clone(stack), dep)
			}

			if colors[dep] == white {
				if cycle := visit(dep); cycle != nil {
					return cycle
				}
			}
		}

		colors[name] = black
		stack = stack[:len(stack)-1]

		return nil
	}

	for _, name := range slices.Sorted(maps.Keys(flow.Actions)) {
		if colors[name] != white {
			continue
		}

		if cycle := visit(name); cycle != nil {
			err := cairnerr.Newf(CodeCycleDetected, "dependency cycle: %s", strings.Join(cycle, " -> "))
			b.add(err.WithDetail("cycle", cycle))

			return
		}
	}
}
