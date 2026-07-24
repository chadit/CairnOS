package chat

import (
	"errors"
	"fmt"
	"strings"
)

// skillListingHeader introduces the description-match section appended to the
// system prompt, telling the driver to load a skill through the skill tool when
// a listed description matches the task.
const skillListingHeader = "Available skills (load one with the skill tool when its description matches the task):"

// SkillInfo is one skill's listing for the system prompt.
type SkillInfo struct {
	Name        string
	Description string
}

// SkillSource lists and loads operator-authored skills. A nil source disables
// both invocation paths, so a node without a skills directory runs chat
// unchanged. The seam keeps internal/chat free of an internal/skills import,
// the same WorkflowTrigger shape the CLI wiring adapts.
type SkillSource interface {
	// List returns the skills available for the system-prompt listing.
	List() []SkillInfo

	// Load returns one skill's body, or ErrSkillNotFound for an unknown name.
	Load(name string) (body string, err error)
}

// loadedSkill is one turn-scoped skill: the invoked name and its body, injected
// into every driver prompt of the invoking turn only.
type loadedSkill struct {
	name string
	body string
}

// slashSkill reports the skill a message invokes through the slash grammar: the
// first whitespace-delimited token, matched whole against ^/[a-z][a-z0-9_-]*$.
// A leading slash, then the skill-name charset, nothing else. So
// /usr/local/bin (a second slash), see /etc/hosts (not the first token),
// /Deploy (uppercase), //x, and a bare / all leave the token inert.
func slashSkill(message string) (string, bool) {
	token := firstToken(message)
	if len(token) < 2 || token[0] != '/' {
		return "", false
	}

	name := token[1:]
	if !skillNameOK(name) {
		return "", false
	}

	return name, true
}

// firstToken returns the first whitespace-delimited token of message, empty
// when the message is blank. A byte scan avoids allocating a Fields slice.
func firstToken(message string) string {
	var start int
	for start < len(message) && isASCIISpace(message[start]) {
		start++
	}

	end := start
	for end < len(message) && !isASCIISpace(message[end]) {
		end++
	}

	return message[start:end]
}

// skillNameOK reports whether name matches ^[a-z][a-z0-9_-]*$, the shared
// skill-name charset the skills library also enforces, so a loadable skill is
// always invokable.
func skillNameOK(name string) bool {
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		return false
	}

	for idx := 1; idx < len(name); idx++ {
		char := name[idx]

		switch {
		case char >= 'a' && char <= 'z':
		case char >= '0' && char <= '9':
		case char == '_' || char == '-':
		default:
			return false
		}
	}

	return true
}

// isASCIISpace reports the ASCII whitespace bytes strings.Fields treats as
// separators, matching the "whitespace-delimited token" grammar for the common
// cases without pulling in unicode classification.
func isASCIISpace(char byte) bool {
	switch char {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	default:
		return false
	}
}

// renderSkillSection renders the leading skill block for a driver prompt when a
// slash invocation loaded one this turn. It is empty when no skill loaded, so
// the prompt layout collapses to context and history.
func renderSkillSection(skill *loadedSkill) string {
	if skill == nil {
		return ""
	}

	return "Skill: " + skill.name + "\n" + skill.body + "\n\n"
}

// appendSkillListing appends the description-match section to the system prompt
// when a source lists at least one skill, so the driver can pick a skill by its
// description and load it through the skill tool. A nil source or an empty list
// returns the prompt unchanged, so the section never shows when there is nothing
// to offer. Rescanned every turn through source.List, so a dropped-in skill
// appears on the next turn with no restart.
func appendSkillListing(system string, source SkillSource) string {
	if source == nil {
		return system
	}

	infos := source.List()
	if len(infos) == 0 {
		return system
	}

	var section strings.Builder

	// Grow once up front so the per-skill writes below never reallocate, the
	// joinHistory sizing idiom. Each line adds "\n- ", the name, and an optional
	// ": " plus description.
	total := len(system) + len("\n\n") + len(skillListingHeader)
	for idx := range infos {
		total += len("\n- ") + len(infos[idx].Name) + len(": ") + len(infos[idx].Description)
	}

	section.Grow(total)

	section.WriteString(system)
	section.WriteString("\n\n")
	section.WriteString(skillListingHeader)

	for idx := range infos {
		section.WriteString("\n- ")
		section.WriteString(infos[idx].Name)

		if infos[idx].Description != "" {
			section.WriteString(": ")
			section.WriteString(infos[idx].Description)
		}
	}

	return section.String()
}

// skillLoadFailureNote is the tool-role text recording a slash invocation that
// did not load. An unknown name reads as unavailable; any other error names the
// failure. Either way the turn proceeds as a plain message, so a skill problem
// never fails a turn.
func skillLoadFailureNote(name string, err error) string {
	if errors.Is(err, ErrSkillNotFound) {
		return fmt.Sprintf("no skill named %q is available", name)
	}

	return fmt.Sprintf("skill %q could not load: %v", name, err)
}
