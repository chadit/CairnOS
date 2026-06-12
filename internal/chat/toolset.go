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
// supplies (memory, vault_query) and the always-present run_workflow gate. A
// nil or unimplemented read tool is skipped, so a node without a vault still
// runs chat with memory and the gate. The run_workflow descriptor is always
// offered, since the gate is the whole point of the chat exception; an
// approval still needs a workflow trigger wired to actually fire.
func BuildToolSet(readTools map[string]runner.Runner) ToolSet {
	tools := make(map[string]runner.Runner, len(readTools))
	specs := make([]runner.ToolSpec, 0, len(readTools)+1)

	// Order the read tools by their well-known names so the descriptor list the
	// driver sees is stable across runs (memory first, then vault_query).
	for _, name := range []string{ToolMemory, ToolVaultQuery} {
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
