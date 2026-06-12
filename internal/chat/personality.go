package chat

import "maps"

// Built-in personality names. A personality is a named system prompt that
// frames every turn in a session (.agents/spec/13-roadmap.md, Phase 9:
// "pluggable personalities (system prompts)"). The config can add or override
// personalities; these two ship so a fresh node has something to pick.
const (
	// PersonalityAssistant is the general helper: answers questions, reads
	// memory and the vault, proposes workflows when asked to do real work.
	PersonalityAssistant = "assistant"

	// PersonalityOps is the operations-leaning personality: terse, action-
	// oriented, quicker to reach for a workflow trigger.
	PersonalityOps = "ops"
)

// DefaultPersonality is the personality a session gets when neither the
// request nor the config names one.
const DefaultPersonality = PersonalityAssistant

// builtinPrompts is the system prompt behind each built-in personality. They
// share the gate framing (the agent proposes, the user confirms) so no
// personality can talk itself past the D9 confirmation step.
//
//nolint:gochecknoglobals // an immutable lookup table read at turn start, never mutated
var builtinPrompts = map[string]string{
	PersonalityAssistant: "You are CairnOS, a helpful assistant inside a workflow engine. " +
		"Answer the user clearly. Use the memory and vault_query tools to look things up when they help. " +
		"When the user wants real work done, propose it with run_workflow; that asks the user to confirm before anything runs.",
	PersonalityOps: "You are CairnOS in operations mode: terse and action-oriented. " +
		"Get to the point. Use memory and vault_query to check facts. " +
		"When work needs doing, reach for run_workflow to propose it; the user confirms before any run starts.",
}

// Personalities is the resolved set of named system prompts a node serves: the
// built-ins plus whatever the config adds, with a default name to fall back on.
// It is the pluggable-personality surface from the roadmap, built once at
// startup and read per turn.
type Personalities struct {
	prompts       map[string]string
	defaultName   string
	defaultPrompt string
}

// NewPersonalities builds the personality set from the built-ins overlaid with
// the config's [chat] personalities, and the configured default name. A config
// entry with the same name as a built-in overrides it, so an operator can
// reshape "assistant" without losing the name. An empty defaultName falls back
// to DefaultPersonality; a default naming an unknown personality still resolves
// because the assistant built-in is always present.
func NewPersonalities(configured map[string]string, defaultName string) *Personalities {
	prompts := make(map[string]string, len(builtinPrompts)+len(configured))

	maps.Copy(prompts, builtinPrompts)

	for name, prompt := range configured {
		if name != "" && prompt != "" {
			prompts[name] = prompt
		}
	}

	if defaultName == "" {
		defaultName = DefaultPersonality
	}

	defaultPrompt, known := prompts[defaultName]
	if !known {
		defaultName = DefaultPersonality
		defaultPrompt = prompts[DefaultPersonality]
	}

	return &Personalities{prompts: prompts, defaultName: defaultName, defaultPrompt: defaultPrompt}
}

// Resolve returns the system prompt for one personality name, falling back to
// the default personality's prompt when the name is empty or unknown, so a
// session never runs without framing. The second result is the name actually
// used, which a session records so its history is honest about which
// personality answered.
func (p *Personalities) Resolve(name string) (string, string) {
	if name == "" {
		return p.defaultPrompt, p.defaultName
	}

	prompt, known := p.prompts[name]
	if !known {
		return p.defaultPrompt, p.defaultName
	}

	return prompt, name
}

// DefaultName reports the personality a new session gets when the request names
// none.
func (p *Personalities) DefaultName() string {
	return p.defaultName
}

// Known reports whether name is a defined personality, so a CreateSession can
// reject a typo'd personality instead of silently falling back.
func (p *Personalities) Known(name string) bool {
	_, ok := p.prompts[name]

	return ok
}
