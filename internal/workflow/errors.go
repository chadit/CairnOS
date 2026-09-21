package workflow

import "errors"

// Sentinel errors for the workflow package. Validation reports rich
// cairnerr.Error values with canonical workflow.* codes; these sentinels cover
// the low-level helpers underneath that machinery.
var (
	// ErrEmptyDuration reports a duration field that was present but blank.
	ErrEmptyDuration = errors.New("empty duration")

	// ErrBadDuration reports a duration string neither the day suffix nor
	// time.ParseDuration could read.
	ErrBadDuration = errors.New("bad duration")

	// ErrBadReference reports an interpolation reference whose shape is wrong
	// (empty, or the wrong number of parts for its kind).
	ErrBadReference = errors.New("malformed interpolation reference")

	// ErrBadExpression reports a condition that failed to lex or parse.
	ErrBadExpression = errors.New("malformed condition expression")

	// ErrEvalType reports a condition that compared incompatible types at
	// evaluation time.
	ErrEvalType = errors.New("type mismatch in condition")

	// ErrUnresolvedRef reports evaluation against a resolver that has no
	// value to substitute into a reference.
	ErrUnresolvedRef = errors.New("unresolved reference")
)
