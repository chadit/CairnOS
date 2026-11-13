package chat

import (
	"context"

	"github.com/chadit/CairnOS/pkg/runner"
)

// RunWorkflowToolName is the registry name of the confirmation-gated workflow trigger the
// chat agent holds. It is the one tool whose call the chat agent
// intercepts instead of executing: a call raises a confirmation gate rather
// than running anything.
const RunWorkflowToolName = "run_workflow"

// run_workflow tool argument keys.
const (
	argWorkflow = "workflow"
	argInputs   = "inputs"
)

// JSON Schema keywords the tools' Describe schemas repeat.
const (
	schemaType        = "type"
	schemaObject      = "object"
	schemaString      = "string"
	schemaDescription = "description"
	schemaProperties  = "properties"
	schemaRequired    = "required"
)

// runWorkflowTool is the descriptor side of the confirmation gate. It implements
// runner.Describer so the driver can see and call it, but Execute never runs a
// workflow: the chat agent intercepts a run_workflow call and turns it into a
// ConfirmationRequired event. Execute fails loudly so a code path calling the
// tool directly cannot bypass the gate.
type runWorkflowTool struct{}

var (
	_ runner.Runner    = (*runWorkflowTool)(nil)
	_ runner.Describer = (*runWorkflowTool)(nil)
)

// newRunWorkflowTool builds the descriptor tool.
func newRunWorkflowTool() *runWorkflowTool {
	return &runWorkflowTool{}
}

// Name implements runner.Runner.
func (*runWorkflowTool) Name() string {
	return RunWorkflowToolName
}

// Validate implements runner.Runner: the workflow argument is required so a
// proposal always names what it would run.
func (*runWorkflowTool) Validate(task *runner.Task) error {
	if !task.Resolved {
		return nil
	}

	name, _ := task.Params[argWorkflow].(string)
	if name == "" {
		return refusal(RunWorkflowToolName+".missing_workflow", "run_workflow requires a workflow name")
	}

	return nil
}

// Execute implements runner.Runner and must never run in normal operation: the
// chat agent intercepts the run_workflow call and raises the confirmation gate
// before any tool execution. Reaching here means the gate was bypassed, so it
// refuses rather than silently firing an unconfirmed workflow.
func (*runWorkflowTool) Execute(ctx context.Context, _ *runner.Task) (runner.Result, error) {
	if err := ctx.Err(); err != nil {
		return runner.Result{}, runner.Wrapf(err, runner.ClassTransient, RunWorkflowToolName+".canceled", "%v", err)
	}

	return runner.Result{}, refusal(RunWorkflowToolName+".gate_bypassed",
		"run_workflow must go through the chat confirmation gate; direct execution is refused")
}

// Describe implements runner.Describer. The description tells the driver that a
// call proposes a workflow for the user to confirm, so the model knows the tool
// is a request, not an immediate action.
func (*runWorkflowTool) Describe() runner.ToolSpec {
	return runner.ToolSpec{
		Name: RunWorkflowToolName,
		Description: "Propose running a CairnOS workflow. " +
			"Calling this does not run anything: it asks the user to confirm the workflow and its inputs first, " +
			"and only an explicit approval triggers the run. Use it when the conversation calls for real work, " +
			"naming the workflow and any inputs it needs.",
		InputSchema: map[string]any{
			schemaType: schemaObject,
			schemaProperties: map[string]any{
				argWorkflow: map[string]any{
					schemaType:        schemaString,
					schemaDescription: "the workflow name to run",
				},
				argInputs: map[string]any{
					schemaType:        schemaObject,
					schemaDescription: "inputs the workflow declares",
				},
			},
			schemaRequired: []string{argWorkflow},
		},
	}
}

// proposalFrom reads a run_workflow tool call's arguments into a Proposal. The
// estimated budget is filled by the caller from the workflow definition or the
// turn cap, since the tool arguments do not carry it.
func proposalFrom(arguments map[string]any) Proposal {
	name, _ := arguments[argWorkflow].(string)
	inputs, _ := arguments[argInputs].(map[string]any)

	return Proposal{WorkflowName: name, Inputs: inputs}
}
