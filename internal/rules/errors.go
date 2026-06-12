package rules

import "errors"

// Sentinel errors for the rules package.
var (
	// ErrUnknownLanguage reports GetRules or ListRules for a language with no
	// rules in the workspace, so a runner can tell "no such language" from an
	// empty merge.
	ErrUnknownLanguage = errors.New("no rules for language")

	// ErrInvalidRule reports a PutRule with an empty language or key, the two
	// fields the (language, project, key) primary key needs.
	ErrInvalidRule = errors.New("rule requires a language and a key")
)
