package builtins

import "errors"

// Construction-time sentinels for runner factories. Classified runner.Error
// values cover execution failures; these cover config decoding, which happens
// before any task exists.
var (
	// ErrConfigValue reports a runner config key holding the wrong type.
	ErrConfigValue = errors.New("invalid runner config value")

	// ErrMalformedRules reports a rules file line that is not a key = value pair.
	ErrMalformedRules = errors.New("malformed rules file")

	// ErrRulesUnknownLanguage reports that a wired RulesLookup has no rules for
	// the requested language. The wiring adapter returns this so the runner can
	// fall back to its static-file dir without importing the rules store
	// package; it is the decoupling seam between this built-in and
	// internal/rules.
	ErrRulesUnknownLanguage = errors.New("rules service has no rules for language")

	// errBlockedTarget is the dial-time sentinel the egress guard wraps when it
	// refuses a destination IP. It rides under the net package's dial error
	// chain, so a runner's transport classifier finds it with errors.Is and
	// reports a blocked_target instead of a generic network failure.
	errBlockedTarget = errors.New("target resolves to a blocked address")

	// errUnmappableValue reports a Go params/inputs value with no Starlark
	// equivalent. The code runner wraps it as code.bad_params. Params are
	// JSON-shaped in practice, so this stays defensive.
	errUnmappableValue = errors.New("value has no starlark equivalent")

	// errBadOutput reports a Starlark output value the code runner refuses to
	// hand downstream: a non-data type, an int outside int64, or a dict with a
	// non-string key. The runner wraps it as code.bad_output. Failing here
	// keeps output types stable across runs instead of letting them drift.
	errBadOutput = errors.New("output is not a json-shaped value")
)
