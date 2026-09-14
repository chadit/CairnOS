package chat

import (
	"github.com/chadit/CairnOS/pkg/runner"
)

// ToolSet is the chat agent's resolved tools: the read tools it runs inline
// (memory, vault_query) keyed by name, plus every tool's descriptor in a
// stable order for the driver. The run_workflow descriptor is in Specs but
// never in Tools, since its call is intercepted into the confirmation gate
// rather than executed (.agents/spec/14-open-decisions.md, D9).
type ToolSet struct {
	// Tools are the executable read tools, keyed by name.
	Tools map[string]runner.Runner

	// Specs are the descriptors handed to the driver, including run_workflow's.
	Specs []runner.ToolSpec
}

// BuildToolSet assembles the chat tool set from the read tools the wiring
// supplies (memory, vault_query) plus the run_workflow gate. A nil or
// unimplemented read tool is skipped, so a node without a vault still runs chat.
// run_workflow is always offered because the gate is the point of the chat
// exception. An approval needs a workflow trigger wired to fire.
func BuildToolSet(readTools map[string]runner.Runner) ToolSet {
	tools := make(map[string]runner.Runner, len(readTools))
	specs := make([]runner.ToolSpec, 0, len(readTools)+1)

	// Order the read tools by their well-known names so the descriptor list the
	// driver sees is stable across runs: memory, then vault_query, then the skill
	// read tool. run_workflow's gate descriptor still goes last, below.
	for _, name := range []string{ToolMemory, ToolVaultQuery, SkillToolName} {
		rnr, present := readTools[name]
		if !present {
			continue
		}

		describer, describable := rnr.(runner.Describer)
		if !describable {
			continue
		}

		tools[name] = rnr

		specs = append(specs, describer.Describe())
	}

	// The gate descriptor goes last so the driver reads the read tools first.
	specs = append(specs, newRunWorkflowTool().Describe())

	return ToolSet{Tools: tools, Specs: specs}
}
