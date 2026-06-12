package workflow

import (
	"maps"
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
	b.checkSchedule(flow)
	b.checkInputs(flow)
	b.checkBudget(flow)

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

// scheduleShorthands are the accepted cron shorthands; anything else must be
// a 5-field cron expression.
const cronFieldCount = 5

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

	if len(strings.Fields(schedule)) != cronFieldCount {
		b.addf(CodeInvalid, "schedule %q must be a 5-field cron expression or an @-shorthand", schedule)
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

	b.checkCondition(action)
	b.checkActionRefs(flow, action)
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

	if action.Runner != "" || action.Params != nil {
		b.addf(CodeAgentFields, "agent action %q cannot use runner or params; those belong to task actions", action.Name)
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
	texts := []string{action.Goal, action.Condition}

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

	walk(action.Params)

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
	case RefEnv, RefSecret:
		// Shape was checked at parse time. Allowlist and secret existence are
		// runtime config concerns, enforced when the engine resolves values.
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
