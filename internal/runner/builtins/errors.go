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
)
