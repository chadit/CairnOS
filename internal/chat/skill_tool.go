package chat

import (
	"context"
	"fmt"

	"github.com/chadit/CairnOS/pkg/runner"
)

// SkillToolName is the registry name of the skill read tool, the
// description-match invocation path alongside the slash grammar. The driver
// calls it to load one operator-authored skill by name. A load influences
// prompt text only: it adds no capability, raises no budget, and cannot move
// the D9 confirmation gate (.agents/spec/parity/skills-context.md, Part 1).
const SkillToolName = "skill"

// skill tool argument and output keys. name is both the required argument and
// the output key echoing which skill loaded; body carries the loaded text.
const (
	argName = "name"
	keyBody = "body"
)

// schemaDescription is the JSON-schema description keyword. A const keeps the
// package under the goconst repeat threshold, since run_workflow_tool.go
// already carries the literal twice.
const schemaDescription = "description"

// skillTool is the descriptor-plus-runner the driver calls to load a skill by
// name. Unlike run_workflow it executes for real rather than raising a gate:
// Execute reads the named skill through the SkillSource and returns its body,
// which runReadTool persists raw so the tool path carries byte-identical text
// to a slash invocation.
type skillTool struct {
	source SkillSource
}

var (
	_ runner.Runner    = (*skillTool)(nil)
	_ runner.Describer = (*skillTool)(nil)
)

// NewSkillTool builds the skill read tool over a skill source. The CLI wiring
// registers it into the chat read tools when a source is configured; a nil
// source leaves the tool out, so a node without a skills directory never offers
// it.
func NewSkillTool(source SkillSource) runner.Runner {
	return &skillTool{source: source}
}

// Name implements runner.Runner.
func (*skillTool) Name() string { return SkillToolName }

// Validate implements runner.Runner: name is required so a load always names a
// skill. Load-time validation (unresolved params) checks structure only.
func (*skillTool) Validate(task *runner.Task) error {
	if !task.Resolved {
		return nil
	}

	if name, _ := task.Params[argName].(string); name == "" {
		return runner.NewError(runner.ClassPermanent, SkillToolName+".missing_name",
			"skill requires a name")
	}

	return nil
}

// Execute loads the named skill's body and returns it as the tool result. An
// unknown name is a permanent skill.not_found: retrying cannot conjure a skill
// the operator never wrote, and executeTool renders it as the usual error
// transcript value the driver reads.
func (s *skillTool) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	if err := ctx.Err(); err != nil {
		return runner.Result{}, runner.NewError(runner.ClassTransient, SkillToolName+".canceled", err.Error())
	}

	name, _ := task.Params[argName].(string)
	if name == "" {
		return runner.Result{}, runner.NewError(runner.ClassPermanent, SkillToolName+".missing_name",
			"skill requires a name")
	}

	body, err := s.source.Load(name)
	if err != nil {
		return runner.Result{}, runner.NewError(runner.ClassPermanent, SkillToolName+".not_found",
			fmt.Sprintf("no skill named %q is available", name))
	}

	return runner.Result{Output: map[string]any{argName: name, keyBody: body}}, nil
}

// Describe implements runner.Describer. The description tells the driver the
// tool loads a listed skill's guidance for this turn, so the model reaches for
// it when a skill's system-prompt description matches the task.
func (*skillTool) Describe() runner.ToolSpec {
	return runner.ToolSpec{
		Name: SkillToolName,
		Description: "Load one operator-authored skill by name and read its body. " +
			"Call it when a listed skill's description matches the task; the body is guidance " +
			"for this turn only, not a new tool, budget, or permission.",
		InputSchema: map[string]any{
			schemaType: schemaObject,
			"properties": map[string]any{
				argName: map[string]any{
					schemaType:        schemaString,
					schemaDescription: "the skill name to load",
				},
			},
			"required": []string{argName},
		},
	}
}

// toolExchangeRecord renders one tool exchange for the transcript. A successful
// skill load is special-cased to the raw-body record so the description-match
// path carries byte-identical text to a slash invocation. Every other tool, and
// a skill error, takes the truncating render that bounds one large value.
func toolExchangeRecord(tool string, arguments map[string]any, output any) string {
	if tool == SkillToolName {
		if record, ok := skillToolRecord(output); ok {
			return record
		}
	}

	return fmt.Sprintf("called %s(%s) -> %s", tool, renderValue(arguments), truncateResult(renderValue(output)))
}

// skillToolRecord renders the persisted record for a successful skill-tool
// result: the raw body, uncut, so the tool path and the slash path carry
// byte-identical skill text (.agents/spec/parity/skills-context.md,
// "Truncation parity"). Load already capped the body at 16 KiB, so the record
// stays bounded. Any other shape, including an error result, reports ok false
// and takes the normal truncating render.
func skillToolRecord(output any) (string, bool) {
	table, isTable := output.(map[string]any)
	if !isTable {
		return "", false
	}

	body, hasBody := table[keyBody].(string)
	if !hasBody {
		return "", false
	}

	name, _ := table[argName].(string)

	return fmt.Sprintf("loaded skill %q (%d bytes) body:\n%s", name, len(body), body), true
}
