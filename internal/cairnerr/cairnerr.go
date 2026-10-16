// Package cairnerr is the one error shape used across the engine, the API,
// and gRPC. Every failure maps to a canonical dotted code
// (.agents/spec/04-execution-engine.md, "Error model").
package cairnerr

import (
	"fmt"
	"maps"
	"strings"
)

// Error is a failure with a stable machine code. Message is safe to surface
// to users; Details carries structured context (the cycle path, the missing
// key) for programmatic consumers.
type Error struct {
	Details map[string]any `json:"details,omitempty"`

	// cause is the underlying failure, kept unexported and out of the wire form:
	// callers across the API and gRPC boundary get the code and the message,
	// while an in-process caller can still reach the original with errors.Is.
	cause error

	Code    string `json:"code"`
	Message string `json:"message"`
}

// New builds an Error with a canonical code and a plain message.
func New(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Newf builds an Error with a formatted message.
func Newf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrapf builds an Error that keeps its cause reachable. Use it whenever the
// underlying error is something a caller might test for, such as a context
// deadline or fs.ErrNotExist; the code stays the discriminator, and the chain
// stops being lost at the package boundary.
func Wrapf(cause error, code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), cause: cause}
}

// Unwrap exposes the wrapped cause to errors.Is and errors.As. It is nil for an
// Error built by New or Newf, which is the shape for a failure this process
// decided on its own.
func (e *Error) Unwrap() error {
	return e.cause
}

func (e *Error) Error() string {
	return e.Code + ": " + e.Message
}

// Equal reports whether two errors carry the same wire form: the code, the
// message, and the structured details. The cause is left out because it is
// process-local and never serialized, so two errors describing one failure can
// hold different chains. reflect.DeepEqual would compare those chains too.
func Equal(first, second *Error) bool {
	if first == nil || second == nil {
		return first == second
	}

	if first.Code != second.Code || first.Message != second.Message {
		return false
	}

	return maps.Equal(detailStrings(first.Details), detailStrings(second.Details))
}

// detailStrings renders details for comparison. Values arrive as any, holding
// whatever a caller attached and whatever a store decoded them back into, so
// they are compared as they would be read rather than by their dynamic type.
func detailStrings(details map[string]any) map[string]string {
	rendered := make(map[string]string, len(details))
	for key, value := range details {
		rendered[key] = fmt.Sprint(value)
	}

	return rendered
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
