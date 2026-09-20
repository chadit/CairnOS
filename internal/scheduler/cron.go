package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// minFireInterval floors how often a schedule may fire. One fire costs a store
// round trip, a queue insert and a workflow run, so a cadence tighter than this
// is a typo rather than a schedule; rejecting it at Add keeps a runaway
// expression out of the store instead of out of the log.
const minFireInterval = 10 * time.Second

// rateProbeFires is how many consecutive fires the rate check measures. A gap
// under the floor repeats once per period, so a handful of fires exposes it
// even when the period itself is a month.
const rateProbeFires = 8

// shorthands maps the workflow schema's @-shorthands onto their 5-field
// equivalents. Translation happens here rather than relying on the cron
// library's descriptors so the accepted surface stays exactly the four the
// schema validates.
//
//nolint:gochecknoglobals // package-scoped lookup table of fixed translations, never mutated
var shorthands = map[string]string{
	"@hourly":  "0 * * * *",
	"@daily":   "0 0 * * *",
	"@weekly":  "0 0 * * 0",
	"@monthly": "0 0 1 * *",
}

// fieldParser accepts 5-field expressions (minute hour dom month dow) and
// 6-field ones carrying a leading seconds field. A 5-field expression parses
// with second zero, which is what it has always meant, so every stored schedule
// keeps its fire times. Descriptors are handled by the shorthand translation
// above.
//
//nolint:gochecknoglobals // the parser is immutable configuration shared by every call
var fieldParser = cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// cronFieldStep names one cron field and the largest step that can still reach
// a second value in it, which is the distance across the field's range.
type cronFieldStep struct {
	name    string
	maxStep int
}

// fieldSteps lists the step limits in six-field order; a five-field expression
// drops the leading seconds entry.
func fieldSteps() []cronFieldStep {
	return []cronFieldStep{
		{name: "seconds", maxStep: 59},
		{name: "minutes", maxStep: 59},
		{name: "hours", maxStep: 23},
		{name: "day of month", maxStep: 30},
		{name: "month", maxStep: 11},
		{name: "day of week", maxStep: 6},
	}
}

// largestStep reports the biggest step in one cron field, and whether it had
// any. A field is a comma list of entries, each optionally ending in /<step>; a
// step that is not a number is left for the cron parser to report.
func largestStep(field string) (int, bool) {
	var largest int

	for entry := range strings.SplitSeq(field, ",") {
		_, rawStep, found := strings.Cut(entry, "/")
		if !found {
			continue
		}

		if step, err := strconv.Atoi(rawStep); err == nil {
			largest = max(largest, step)
		}
	}

	return largest, largest > 0
}

// checkSteps rejects a step wider than the field it steps through. The cron
// library accepts one and quietly fires on the range's first value alone, so
// "*/75 * * * * *" would silently mean "once a minute" rather than fail.
func checkSteps(expr string) error {
	fields := strings.Fields(expr)

	limits := fieldSteps()
	if len(fields) < len(limits) {
		limits = limits[len(limits)-len(fields):]
	}

	for idx, field := range fields {
		step, stepped := largestStep(field)
		if stepped && step > limits[idx].maxStep {
			return fmt.Errorf("%w: %q: step %d is wider than the %s range",
				ErrBadCron, expr, step, limits[idx].name)
		}
	}

	return nil
}

// rateProbeStart is the instant checkFireRate walks from: a minute boundary, so
// the walk starts on a fire rather than partway between two and never measures
// the distance from an arbitrary now.
func rateProbeStart() time.Time {
	return time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
}

// checkFireRate reports an expression whose fires land closer together than
// minFireInterval. Only the optional seconds field can produce such a gap, so
// this is the guard that makes the field safe to accept.
func checkFireRate(expr string, sched cron.Schedule) error {
	mark := sched.Next(rateProbeStart())

	for range rateProbeFires {
		next := sched.Next(mark)
		if next.IsZero() {
			return nil
		}

		if gap := next.Sub(mark); gap < minFireInterval {
			return fmt.Errorf("%w: %q fires every %s, and the floor is %s", ErrTooFrequent, expr, gap, minFireInterval)
		}

		mark = next
	}

	return nil
}

// parseCron translates an @-shorthand, parses the resulting expression, and
// holds it to the fire-rate floor, wrapping each failure in its own sentinel so
// callers can tell a malformed expression from an overly eager one.
func parseCron(expr string) (cron.Schedule, error) {
	if translated, ok := shorthands[expr]; ok {
		expr = translated
	}

	parsed, err := fieldParser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %w", ErrBadCron, expr, err)
	}

	if err := checkSteps(expr); err != nil {
		return nil, err
	}

	if err := checkFireRate(expr, parsed); err != nil {
		return nil, err
	}

	return parsed, nil
}

// ValidateCron reports whether expr is a supported @-shorthand, a 5-field cron
// expression, or a 6-field one whose leading field is seconds, and whether it
// stays inside the fire-rate floor; Add calls it so bad expressions never
// persist.
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
