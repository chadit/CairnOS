package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/pkg/runner"
)

// keyError is the output key a tool result carries when it failed, so the
// driver sees the failure as data it can react to. keyCode carries the
// classified runner error's stable code alongside it, so the transcript names
// which failure it was (skill.not_found, memory.bad_op) rather than the message
// text alone.
const (
	keyError = "error"
	keyCode  = "code"
)

// The whole conversation history replays into the turn's prompt, so context
// persists across turns. The turn drives the configured ToolCaller the same way
// an agent action does: each iteration the driver answers, calls a read tool that
// runs inline, or proposes a workflow. A run_workflow call is intercepted into
// the D9 confirmation gate and the turn pauses for ConfirmAction. The user
// message and the assistant reply or proposal persist for the next turn.

// SendMessage runs one conversation turn for a new user message and streams its
// events to out, closing out exactly once on return
// (.agents/spec/13-roadmap.md, Phase 9). The caller ranges out until close and
// reads the returned error for the turn's outcome.
func (a *Agent) SendMessage(ctx context.Context, sessionID, userMessage string, out chan<- Event) error {
	defer close(out)

	session, err := a.cfg.Store.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("send to chat session: %w", err)
	}

	if err := a.cfg.Store.AppendMessage(ctx, &store.ChatMessage{
		SessionID: sessionID,
		Role:      store.ChatRoleUser,
		Content:   userMessage,
	}); err != nil {
		return fmt.Errorf("persist user message: %w", err)
	}

	systemPrompt, _ := a.cfg.Personalities.Resolve(session.Personality)
	systemPrompt = appendSkillListing(systemPrompt, a.cfg.Skills)

	turn := &chatTurn{
		agent:   a,
		out:     out,
		session: session,
		system:  systemPrompt,
	}

	if err := turn.loadContext(ctx); err != nil {
		return err
	}

	if err := turn.inlineFileRefs(ctx, userMessage); err != nil {
		return err
	}

	if err := turn.loadSlashSkill(ctx, userMessage); err != nil {
		return err
	}

	return turn.run(ctx)
}

// chatTurn is one turn's mutable state. A single goroutine owns it for the
// whole turn, so no lock guards the fields. ctx is threaded through the methods
// rather than held on the struct.
type chatTurn struct {
	agent   *Agent
	out     chan<- Event
	session *store.ChatSession

	// skill is the turn-scoped skill a slash invocation loaded, rendered into
	// every driver prompt of this turn; nil when none loaded.
	skill *loadedSkill

	system string

	// contextDoc is the per-workspace context doc read once at the top of the
	// turn, rendered into every driver prompt of this turn; empty when no doc
	// loaded or the feature is off.
	contextDoc string

	usage runner.Usage
}

// run drives driver iterations until a final answer, a workflow proposal, or a
// wall (iterations or budget). It returns nil on a normal answer or a raised
// confirmation gate; a non-nil error is a turn failure.
func (t *chatTurn) run(ctx context.Context) error {
	for iteration := 1; iteration <= maxTurnIterations; iteration++ {
		result, err := t.callDriver(ctx)
		if err != nil {
			return err
		}

		done, err := t.applyDecision(ctx, result)
		if err != nil {
			return err
		}

		if done {
			return nil
		}

		if err := t.checkBudget(iteration); err != nil {
			return err
		}
	}

	return errf(CodeMaxIterations,
		"chat turn used all %d driver iterations without an answer", maxTurnIterations)
}

// callDriver runs one driver turn over the freshly assembled prompt. The
// history reloads every iteration so a tool result the agent just recorded is
// visible on its next turn.
func (t *chatTurn) callDriver(ctx context.Context) (runner.Result, error) {
	prompt, err := t.prompt(ctx)
	if err != nil {
		return runner.Result{}, err
	}

	task := &runner.Task{
		RunID:      t.session.ID,
		ActionName: "chat",
		Params: map[string]any{
			paramSystem: t.system,
			paramPrompt: prompt,
		},
		Resolved: true,
	}

	result, err := t.agent.cfg.Driver.ExecuteWithTools(ctx, task, t.agent.cfg.Specs)
	t.usage = addUsage(t.usage, result.Usage)

	if err != nil {
		return runner.Result{}, errf(CodeDriverFailed, "chat driver failed: %v", err)
	}

	return result, nil
}

// applyDecision acts on one interpreted driver turn. done reports the turn
// ended (an answer or a raised gate).
func (t *chatTurn) applyDecision(ctx context.Context, result runner.Result) (bool, error) {
	tool, arguments, isToolCall := readToolCall(&result)
	if !isToolCall {
		return true, t.finishAnswer(ctx, result)
	}

	if tool == RunWorkflowToolName {
		return true, t.raiseGate(ctx, arguments)
	}

	return false, t.runReadTool(ctx, tool, arguments)
}

// finishAnswer persists the assistant reply, streams it, and closes the turn
// with a turn-done event carrying the final text.
func (t *chatTurn) finishAnswer(ctx context.Context, result runner.Result) error {
	answer := renderAnswer(result.Output)

	if err := t.agent.cfg.Store.AppendMessage(ctx, &store.ChatMessage{
		SessionID: t.session.ID,
		Role:      store.ChatRoleAssistant,
		Content:   answer,
	}); err != nil {
		return fmt.Errorf("persist assistant reply: %w", err)
	}

	if err := emit(ctx, t.out, &Event{Type: EventAssistantChunk, Text: answer}); err != nil {
		return err
	}

	return emit(ctx, t.out, &Event{Type: EventTurnDone, Text: answer, Usage: t.usage})
}

// runReadTool executes one allowed read tool inline, streams a tool-call event,
// persists the exchange as a tool-role message so it replays into the next
// turn, and lets the loop continue. A tool that is not in
// the allowlist or fails surfaces its outcome into the transcript rather than
// failing the turn, so the driver can adapt.
func (t *chatTurn) runReadTool(ctx context.Context, tool string, arguments map[string]any) error {
	if err := emit(ctx, t.out, &Event{Type: EventToolCall, Tool: tool, ToolInput: arguments}); err != nil {
		return err
	}

	output := t.executeTool(ctx, tool, arguments)

	record := toolExchangeRecord(tool, arguments, output)

	if err := t.agent.cfg.Store.AppendMessage(ctx, &store.ChatMessage{
		SessionID: t.session.ID,
		Role:      store.ChatRoleTool,
		Content:   record,
	}); err != nil {
		return fmt.Errorf("persist tool exchange: %w", err)
	}

	return nil
}

// executeTool runs one read tool through its runner contract: Validate, then a
// single Execute. A disallowed tool or a failure becomes a transcript value the
// driver reads, never a turn failure, matching the agent loop's stance.
func (t *chatTurn) executeTool(ctx context.Context, tool string, arguments map[string]any) any {
	rnr, allowed := t.agent.cfg.Tools[tool]
	if !allowed {
		return map[string]any{keyError: fmt.Sprintf("tool %q is not available", tool)}
	}

	task := &runner.Task{
		RunID:      t.session.ID,
		ActionName: "chat",
		Params:     maps.Clone(arguments),
		Resolved:   true,
	}

	if err := rnr.Validate(task); err != nil {
		return map[string]any{keyError: fmt.Sprintf("tool %q rejected the arguments: %v", tool, err)}
	}

	result, err := rnr.Execute(ctx, task)
	t.usage = addUsage(t.usage, result.Usage)

	if err != nil {
		return toolError(err)
	}

	return result.Output
}

// toolError renders a tool failure as a transcript value the driver reads. It
// carries the message and, for a classified runner error, the stable code, so
// the durable transcript names the failure (the skill tool's skill.not_found,
// for one) rather than showing the message alone.
func toolError(err error) map[string]any {
	out := map[string]any{keyError: err.Error()}

	if classified, ok := errors.AsType[*runner.Error](err); ok {
		out[keyCode] = classified.Code
	}

	return out
}

// raiseGate is the D9 confirmation step: a run_workflow call does not run a
// workflow. It records a pending proposal, persists it as a tool message so the
// thread shows what was proposed, and emits the confirmation-required event,
// ending the turn. Only a later ConfirmAction(approve) fires the run.
func (t *chatTurn) raiseGate(ctx context.Context, arguments map[string]any) error {
	proposal := proposalFrom(arguments)
	proposal.EstimatedBudgetUSD = t.agent.cfg.TurnBudgetUSD

	t.agent.setPending(t.session.ID, &pendingProposal{
		proposal:  proposal,
		sessionID: t.session.ID,
		raisedAt:  time.Now(),
	})

	record := fmt.Sprintf("proposed workflow %q with inputs %s (awaiting confirmation)",
		proposal.WorkflowName, renderValue(proposal.Inputs))

	if err := t.agent.cfg.Store.AppendMessage(ctx, &store.ChatMessage{
		SessionID: t.session.ID,
		Role:      store.ChatRoleTool,
		Content:   record,
	}); err != nil {
		return fmt.Errorf("persist proposal: %w", err)
	}

	clone := proposal

	if err := emit(ctx, t.out, &Event{Type: EventConfirmationRequired, Proposal: &clone}); err != nil {
		return err
	}

	return emit(ctx, t.out, &Event{Type: EventTurnDone, Usage: t.usage})
}

// checkBudget ends the turn when the estimated driver-and-tool spend reaches
// the turn cap. A zero cap leaves the turn uncapped. The check uses >= because
// a spend that reached the cap leaves nothing for another driver call.
func (t *chatTurn) checkBudget(iteration int) error {
	if t.agent.cfg.TurnBudgetUSD <= 0 {
		return nil
	}

	if t.usage.CostUSD >= t.agent.cfg.TurnBudgetUSD {
		return errf(CodeBudgetExhausted,
			"chat turn spent an estimated $%.4f of its $%.2f budget after %d iteration(s)",
			t.usage.CostUSD, t.agent.cfg.TurnBudgetUSD, iteration)
	}

	return nil
}

// loadSlashSkill resolves a leading /name to a turn-scoped skill and records a
// tool-role note. An inert message or nil Skills source is a no-op. A load
// failure goes in the note and the turn proceeds as a plain message, so a skill
// problem never fails a turn. The persisted user message keeps the raw /name
// text untouched.
func (t *chatTurn) loadSlashSkill(ctx context.Context, userMessage string) error {
	name, ok := slashSkill(userMessage)
	if !ok || t.agent.cfg.Skills == nil {
		return nil
	}

	body, err := t.agent.cfg.Skills.Load(name)
	if err != nil {
		return t.persistToolNote(ctx, skillLoadFailureNote(name, err))
	}

	if err := t.persistToolNote(ctx, fmt.Sprintf("loaded skill %q for this turn", name)); err != nil {
		return err
	}

	t.skill = &loadedSkill{name: name, body: body}

	return nil
}

// persistToolNote records one tool-role line so the durable history stays
// honest about what the driver saw this turn: a loaded (or failed) skill, an
// inlined (or refused) @-file reference, or the @-reference limit note.
func (t *chatTurn) persistToolNote(ctx context.Context, note string) error {
	if err := t.agent.cfg.Store.AppendMessage(ctx, &store.ChatMessage{
		SessionID: t.session.ID,
		Role:      store.ChatRoleTool,
		Content:   note,
	}); err != nil {
		return fmt.Errorf("persist tool note: %w", err)
	}

	return nil
}

// prompt assembles one driver turn's user text: an optional leading skill
// section a slash invocation loaded this turn, then the persisted conversation
// so far, rendered as plain text so every ToolCaller sees the same input
// regardless of its wire format.
func (t *chatTurn) prompt(ctx context.Context) (string, error) {
	messages, err := t.agent.cfg.Store.ListMessages(ctx, t.session.ID)
	if err != nil {
		return "", fmt.Errorf("load chat history: %w", err)
	}

	return renderSkillSection(t.skill) + renderContextSection(t.contextDoc) +
		"Conversation so far:\n" + joinHistory(messages), nil
}

// readToolCall reads the driver convention out of one result: a tool call when
// the stop reason or output shape says so, a final answer otherwise. It mirrors
// internal/agentloop's interpret over the public runner contract.
func readToolCall(result *runner.Result) (string, map[string]any, bool) {
	name, arguments, parses := parseToolCall(result.Output)

	switch result.Meta[runner.MetaStopReason] {
	case runner.StopReasonEndTurn:
		return "", nil, false
	case runner.StopReasonToolUse:
		return name, arguments, parses
	default:
		return name, arguments, parses
	}
}

// parseToolCall reads the convention's tool-call shape from a driver output: a
// map with a non-empty "tool" string and an optional "arguments" map.
func parseToolCall(output any) (string, map[string]any, bool) {
	table, isTable := output.(map[string]any)
	if !isTable {
		return "", nil, false
	}

	name, isText := table[runner.ToolCallName].(string)
	if !isText || name == "" {
		return "", nil, false
	}

	raw, present := table[runner.ToolCallArguments]
	if !present || raw == nil {
		return name, map[string]any{}, true
	}

	arguments, isArgs := raw.(map[string]any)
	if !isArgs {
		return "", nil, false
	}

	return name, arguments, true
}

// renderAnswer turns a final driver output into reply text: a string passes
// through, anything else renders as JSON so a structured answer stays readable.
func renderAnswer(output any) string {
	if text, isText := output.(string); isText {
		return text
	}

	return renderValue(output)
}

// renderValue turns a value into text, JSON first with %v as the fallback.
func renderValue(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}

	return string(encoded)
}

// addUsage sums two usage reports field-wise, leaving latency alone.
func addUsage(total, delta runner.Usage) runner.Usage {
	total.InputTokens += delta.InputTokens
	total.OutputTokens += delta.OutputTokens
	total.CachedTokens += delta.CachedTokens
	total.CostUSD += delta.CostUSD

	return total
}
