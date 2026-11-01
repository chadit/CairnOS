// Package workflow implements the CairnOS workflow file format: TOML parsing,
// the depends_on DAG, interpolation references, the condition expression
// grammar, and load-time validation.
package workflow

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const hoursPerDay = 24

// ParseDuration parses the duration strings workflow files use. It accepts
// everything time.ParseDuration does plus a day suffix ("90d"), which config
// values like retention windows need and the standard library refuses.
func ParseDuration(text string) (time.Duration, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, ErrEmptyDuration
	}

	if days, ok := strings.CutSuffix(trimmed, "d"); ok {
		// Only treat it as days when the prefix is a bare integer, so a
		// malformed value like "1h30d" still fails instead of half-parsing.
		count, err := strconv.Atoi(days)
		if err == nil {
			return time.Duration(count) * hoursPerDay * time.Hour, nil
		}
	}

	parsed, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("%w %q: %w", ErrBadDuration, text, err)
	}

	return parsed, nil
}
