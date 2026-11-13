package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// maxTranscriptValue bounds how much of one tool result or argument set
// renders back into the prompt, so a huge tool output cannot inflate every
// following driver call.
const maxTranscriptValue = 4096

// systemPrompt picks the action's framing: its own system_prompt or the
// loop default. References in system_prompt do not interpolate; only the
// goal carries interpolation.
func (r *loopRun) systemPrompt() string {
	if r.action.SystemPrompt != "" {
		return r.action.SystemPrompt
	}

	return defaultSystemPrompt
}

// prompt assembles one driver turn's user text: the goal, the memory
// section when present, and the transcript so far. Rendered as plain text
// rather than provider-native message history so every ToolCaller sees the
// same input regardless of its wire format.
func (r *loopRun) prompt(memorySection string) string {
	var text strings.Builder

	text.WriteString("Goal: ")
	text.WriteString(r.action.Goal)

	if memorySection != "" {
		text.WriteString("\n\nMemory:\n")
		text.WriteString(memorySection)
	}

	if len(r.transcript) > 0 {
		text.WriteString("\n\nTranscript so far:\n")
		text.WriteString(strings.Join(r.transcript, "\n"))
	}

	return text.String()
}

// memorySection loads and renders the action's memory_scope entries,
// token-budgeted through the store's Load contract. No scope or no store
// means no section; a load failure fails the action, because silently
// dropping memory the workflow asked for would be a surprise.
func (r *loopRun) memorySection(ctx context.Context) (string, error) {
	if r.action.MemoryScope == "" || r.loop.memory == nil {
		return "", nil
	}

	entries, err := r.loop.memory.Load(ctx, r.action.MemoryScope, memoryTokenBudget)
	if err != nil {
		return "", fmt.Errorf("load memory scope %q: %w", r.action.MemoryScope, err)
	}

	if len(entries) == 0 {
		return "", nil
	}

	var total int

	for idx := range entries {
		total += len(entries[idx].Content)
	}

	var section strings.Builder

	// Grow once up front: per-entry writes must not reallocate the builder.
	section.Grow(total + len(entries)*3)

	for idx := range entries {
		section.WriteString("- ")
		section.WriteString(entries[idx].Content)
		section.WriteByte('\n')
	}

	return section.String(), nil
}

// appendNote adds one transcript line.
func (r *loopRun) appendNote(note string) {
	r.transcript = append(r.transcript, note)
}

// renderValue turns a value into transcript text, JSON first with %v as
// the fallback, truncated to keep one huge value from dominating the
// prompt.
func renderValue(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return truncate(fmt.Sprintf("%v", value))
	}

	return truncate(string(encoded))
}

// truncate caps transcript text at maxTranscriptValue bytes. Cutting on a
// byte boundary may split a rune; the transcript is prompt text where a
// mangled trailing character costs nothing.
func truncate(text string) string {
	if len(text) <= maxTranscriptValue {
		return text
	}

	return text[:maxTranscriptValue] + "... (truncated)"
}
