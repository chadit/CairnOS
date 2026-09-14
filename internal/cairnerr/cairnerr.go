// Package cairnerr is the one error shape used across the engine, the API,
// and gRPC. Every failure maps to a canonical dotted code
// (.agents/spec/04-execution-engine.md, "Error model").
package cairnerr

import (
	"fmt"
	"strings"
)

// Error is a failure with a stable machine code. Message is safe to surface
// to users; Details carries structured context (the cycle path, the missing
// key) for programmatic consumers.
type Error struct {
	Details map[string]any `json:"details,omitempty"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
}

// New builds an Error with a canonical code and a plain message.
func New(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Newf builds an Error with a formatted message.
func Newf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func (e *Error) Error() string {
	return e.Code + ": " + e.Message
}

// WithDetail attaches one structured detail and returns the error for
// chaining.
func (e *Error) WithDetail(key string, value any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}

	e.Details[key] = value

	return e
}

// Errors aggregates several errors so validation can report everything wrong
// with a file in one pass instead of stopping at the first problem.
type Errors []*Error

func (l Errors) Error() string {
	if len(l) == 0 {
		return "no errors"
	}

	parts := make([]string, len(l))
	for i, e := range l {
		parts[i] = e.Error()
	}

	return fmt.Sprintf("%d error(s): %s", len(l), strings.Join(parts, "; "))
}

// OrNil returns the list as an error, or nil when it is empty, so callers can
// end validation with a single expression instead of a length check.
func (l Errors) OrNil() error {
	if len(l) == 0 {
		return nil
	}

	return l
}
