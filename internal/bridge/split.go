package bridge

import (
	"strings"
	"unicode/utf8"
)

// paragraphBreak is the blank-line boundary splitMessage prefers to break on.
const paragraphBreak = "\n\n"

// splitMessage breaks text into chunks no longer than limit runes, preferring
// blank-line boundaries and hard-cutting a paragraph that exceeds the limit
// alone.
//
// The limit counts runes, not bytes, because that is what the transports
// enforce: Mattermost's MaxPostSize is a rune count and Telegram's 4096 is a
// character limit, so a byte count would truncate a multibyte reply early.
func splitMessage(text string, limit int) []string {
	// A transport that reports a non-positive cap has told us nothing usable: a
	// Mattermost server answering MaxPostSize 0 reaches here. Cutting on it would
	// loop forever appending empty chunks, or panic on a negative slice bound, and
	// both happen inside a conversation worker that Drain then waits on.
	if limit <= 0 {
		return []string{text}
	}

	if utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}

	return packUnits(paragraphUnits(text, limit), limit)
}

// paragraphUnits splits text on blank lines and hard-cuts any paragraph longer
// than limit, so every returned unit fits in one message.
func paragraphUnits(text string, limit int) []string {
	var units []string

	for paragraph := range strings.SplitSeq(text, paragraphBreak) {
		units = appendCut(units, paragraph, limit)
	}

	return units
}

// appendCut appends paragraph to units, hard-cutting it on rune boundaries so no
// unit exceeds limit and no multibyte character is split in half.
func appendCut(units []string, paragraph string, limit int) []string {
	rest := []rune(paragraph)

	for len(rest) > limit {
		units = append(units, string(rest[:limit]))
		rest = rest[limit:]
	}

	if len(rest) > 0 {
		units = append(units, string(rest))
	}

	return units
}

// packUnits greedily joins units back with blank lines while they fit under
// limit, so a long turn arrives as the fewest ordered messages.
func packUnits(units []string, limit int) []string {
	var (
		parts   []string
		current string
	)

	for _, unit := range units {
		candidate := joinChunk(current, unit)

		if utf8.RuneCountInString(candidate) > limit {
			parts = append(parts, current)
			current = unit

			continue
		}

		current = candidate
	}

	if current != "" {
		parts = append(parts, current)
	}

	return parts
}

// joinChunk appends unit to current with a blank-line separator, or returns unit
// alone when current is empty.
func joinChunk(current, unit string) string {
	if current == "" {
		return unit
	}

	return current + paragraphBreak + unit
}
