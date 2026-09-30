package config

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/chadit/CairnOS/internal/workflow"
)

// [update] keys. The parser matches against these and rejects anything else,
// so a typo'd auto fails Load instead of leaving a host that looks opted in and
// never updates.
const (
	updateKeyAuto     = "auto"
	updateKeyInterval = "interval"
)

// defaultUpdateInterval is how often serve asks for a newer build once [update]
// auto is on. One check is a git fetch or a release API call, so a quarter hour
// keeps a host close behind main without leaning on GitHub.
const defaultUpdateInterval = 15 * time.Minute

// Update is the [update] block: whether serve keeps its own build current
// instead of waiting for someone to run `cairnos update`.
type Update struct {
	// Interval is how often serve compares the build it runs against the one an
	// update would install. It must be positive.
	Interval time.Duration

	// Auto turns the serve loop on. Default false, because the loop ends the
	// process to restart it on the new build, which an operator opts into.
	Auto bool
}

// parseUpdateBlock applies the verbatim [update] table over the defaults block
// already holds. The table arrives raw because go-toml drops an unknown struct
// field silently, the same trap [secrets.bitwarden] parses around.
func parseUpdateBlock(block *Update, raw map[string]any) error {
	for _, key := range slices.Sorted(maps.Keys(raw)) {
		if err := applyUpdateKey(block, key, raw[key]); err != nil {
			return err
		}
	}

	return nil
}

// applyUpdateKey sets one key or returns an error naming it.
func applyUpdateKey(block *Update, key string, value any) error {
	switch key {
	case updateKeyAuto:
		auto, isBool := value.(bool)
		if !isBool {
			return fmt.Errorf("%w: update.auto must be true or false", ErrInvalidConfig)
		}

		block.Auto = auto

		return nil
	case updateKeyInterval:
		return parseUpdateInterval(block, value)
	default:
		return fmt.Errorf("%w: update has unknown key %q", ErrInvalidConfig, key)
	}
}

// parseUpdateInterval reads the interval through the parser every other config
// duration goes through, so "1d" means here what it means in [retention].
func parseUpdateInterval(block *Update, value any) error {
	text, isString := value.(string)
	if !isString {
		return fmt.Errorf("%w: update.interval must be a duration string such as \"15m\"", ErrInvalidConfig)
	}

	interval, err := workflow.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("%w: update.interval: %w", ErrInvalidConfig, err)
	}

	block.Interval = interval

	return nil
}

// validateUpdate rejects an interval that is not positive whether auto is on or
// off: the loop's ticker cannot run on one, and a zero written by hand is a
// typo rather than a way to switch the loop off.
func validateUpdate(block *Update) error {
	if block.Interval <= 0 {
		return fmt.Errorf("%w: update.interval must be positive, got %s", ErrInvalidConfig, block.Interval)
	}

	return nil
}
