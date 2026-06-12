package scheduler

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// shorthands maps the workflow schema's @-shorthands onto their 5-field
// equivalents (.agents/spec/02-workflow-schema.md). Translation happens here
// rather than relying on the cron library's descriptors so the accepted
// surface stays exactly the four the schema validates.
//
//nolint:gochecknoglobals // package-scoped lookup table of fixed translations, never mutated
var shorthands = map[string]string{
	"@hourly":  "0 * * * *",
	"@daily":   "0 0 * * *",
	"@weekly":  "0 0 * * 0",
	"@monthly": "0 0 1 * *",
}

// fieldParser accepts strictly 5-field expressions (minute hour dom month
// dow); descriptors are handled by the shorthand translation above.
//
//nolint:gochecknoglobals // the parser is immutable configuration shared by every call
var fieldParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// parseCron translates an @-shorthand and parses the resulting 5-field
// expression, wrapping failures in ErrBadCron so callers test one sentinel.
func parseCron(expr string) (cron.Schedule, error) {
	if translated, ok := shorthands[expr]; ok {
		expr = translated
	}

	parsed, err := fieldParser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %w", ErrBadCron, expr, err)
	}

	return parsed, nil
}

// ValidateCron reports whether expr is a supported @-shorthand or a valid
// 5-field cron expression; Add calls it so bad expressions never persist.
func ValidateCron(expr string) error {
	_, err := parseCron(expr)

	return err
}

// NextFire computes the first fire time strictly after from for expr.
func NextFire(expr string, from time.Time) (time.Time, error) {
	parsed, err := parseCron(expr)
	if err != nil {
		return time.Time{}, err
	}

	return parsed.Next(from), nil
}
