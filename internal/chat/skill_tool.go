package chat

import (
	"context"
	"fmt"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// SkillToolName is the registry name of the skill read tool, the
// description-match invocation path alongside the slash grammar. The driver
// calls it to load one operator-authored skill by name. A load influences
// prompt text only: it adds no capability, raises no budget, and cannot move
// the D9 confirmation gate.
const SkillToolName = "skill"

// skill tool argument and output keys. name is both the required argument and
// the output key echoing which skill loaded; body carries the loaded text.
const (
	argName = "name"
	keyBody = "body"
)

// SkillTool is the descriptor-plus-runner the driver calls to load a skill by
// name. Unlike run_workflow it executes for real rather than raising a gate:
// Execute reads the named skill through the SkillSource and returns its body,
// which runReadTool persists raw so the tool path carries byte-identical text
// to a slash invocation.
type SkillTool struct {
	source SkillSource
}

var (
	_ runner.Runner    = (*SkillTool)(nil)
	_ runner.Describer = (*SkillTool)(nil)
)

// NewSkillTool builds the skill read tool over a skill source. The CLI wiring
// registers it into the chat read tools when a source is configured; a nil
// source leaves the tool out, so a node without a skills directory never offers
// it.
func NewSkillTool(source SkillSource) *SkillTool {
	return &SkillTool{source: source}
}

// Name implements runner.Runner.
func (*SkillTool) Name() string { return SkillToolName }

// Validate implements runner.Runner: name is required so a load always names a
// skill. Load-time validation (unresolved params) checks structure only.
func (*SkillTool) Validate(task *runner.Task) error {
	if !task.Resolved {
		return nil
	}

	if name, _ := task.Params[argName].(string); name == "" {
		return refusal(SkillToolName+".missing_name", "skill requires a name")
	}

	return nil
}

// Execute loads the named skill's body and returns it as the tool result. An
// unknown name is a permanent skill.not_found: retrying cannot conjure a skill
// the operator never wrote, and executeTool renders it as the usual error
// transcript value the driver reads.
func (s *SkillTool) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	if err := ctx.Err(); err != nil {
		return runner.Result{}, runner.Wrapf(err, runner.ClassTransient, SkillToolName+".canceled", "%v", err)
	}

	name, _ := task.Params[argName].(string)
	if name == "" {
		return runner.Result{}, refusal(SkillToolName+".missing_name", "skill requires a name")
	}

	body, err := s.source.Load(name)
	if err != nil {
		return runner.Result{}, runner.Wrapf(err, runner.ClassPermanent, SkillToolName+".not_found",
			"no skill named %q is available", name)
	}

	return runner.Result{Output: map[string]any{argName: name, keyBody: body}}, nil
}

// Describe implements runner.Describer. The description tells the driver the
// tool loads a listed skill's guidance for this turn, so the model reaches for
// it when a skill's system-prompt description matches the task.
func (*SkillTool) Describe() runner.ToolSpec {
	return runner.ToolSpec{
		Name: SkillToolName,
		Description: "Load one operator-authored skill by name and read its body. " +
			"Call it when a listed skill's description matches the task; the body is guidance " +
			"for this turn only, not a new tool, budget, or permission.",
		InputSchema: map[string]any{
			schemaType: schemaObject,
			schemaProperties: map[string]any{
				argName: map[string]any{
					schemaType:        schemaString,
					schemaDescription: "the skill name to load",
				},
			},
			schemaRequired: []string{argName},
		},
	}
}

// toolExchangeRecord renders one tool exchange for the transcript. A successful
// skill load keeps its raw body so the description-match path matches a slash
// invocation byte for byte, and a usage report keeps its table unescaped so the
// reply can carry it verbatim. Everything else takes the truncating JSON render.
func toolExchangeRecord(tool string, arguments map[string]any, output any) string {
	switch tool {
	case SkillToolName:
		if record, ok := skillToolRecord(output); ok {
			return record
		}
	case ToolUsage:
		if text, ok := builtins.UsageToolText(output); ok {
			return fmt.Sprintf("called %s(%s) ->\n%s", tool, renderValue(arguments), truncateResult(text))
		}
	}

	return fmt.Sprintf("called %s(%s) -> %s", tool, renderValue(arguments), truncateResult(renderValue(output)))
}

// skillToolRecord renders the persisted record for a successful skill-tool
// result: the raw body, uncut, so the tool path and the slash path carry
// byte-identical skill text. Load already capped the body at 16 KiB, so the
// record stays bounded. Any other shape, including an error result, reports ok
// false and takes the normal truncating render.
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
