package runner

import (
	"errors"
	"time"
)

// Registry sentinels.
var (
	// ErrDuplicateRunner reports a second registration under the same name.
	ErrDuplicateRunner = errors.New("runner already registered")

	// ErrUnknownRunner reports a lookup or build for a name never registered.
	ErrUnknownRunner = errors.New("unknown runner")
)

// ErrorClass tells the engine whether retrying can help
// (.agents/spec/03-runner-interface.md, "Errors and retry classification").
type ErrorClass int

const (
	// ClassTransient marks failures that are safe to retry on an idempotent
	// action: network blips, 429s, 503s, timeouts. The engine applies backoff.
	ClassTransient ErrorClass = iota
	// ClassPermanent marks failures retrying cannot fix: bad params, 400s,
	// auth failures. A plain non-Error error is treated as permanent too, so
	// the engine never repeats a side effect a runner did not declare safe.
	ClassPermanent
	// ClassRateLimited behaves like transient, but the engine prefers the
	// RetryAfter hint over computed backoff.
	ClassRateLimited
	// ClassNeedsReauth marks an expired credential that cannot self-refresh, such
	// as a dead OAuth refresh token on a subscription CLI tool. The engine cannot
	// fix it by retrying and a human must sign in again, so it is non-retryable
	// like permanent; the scheduler queue holds the job until re-auth instead of
	// dead-lettering it. See D12 in .agents/spec/14-open-decisions.md.
	ClassNeedsReauth
)

// Error is the classified failure a runner returns so the engine can decide
// retry behavior and surface a stable code.
type Error struct {
	Cause      error  // wrapped underlying error
	Code       string // stable machine code, e.g. "openai.rate_limited"
	Message    string // human message, safe to log
	Class      ErrorClass
	RetryAfter time.Duration // optional hint when Class is ClassRateLimited
}

// NewError builds a classified runner error.
func NewError(class ErrorClass, code, message string) *Error {
	return &Error{Class: class, Code: code, Message: message}
}

// NewReauthError builds a NeedsReauth runner error for a credential a human must
// refresh. message should name the remediation (e.g. "run: codex login").
func NewReauthError(code, message string) *Error {
	return &Error{Class: ClassNeedsReauth, Code: code, Message: message}
}

// IsReauth reports whether err is, or wraps, a runner error that needs re-auth,
// so the scheduler worker can hold the job instead of dead-lettering it.
func IsReauth(err error) bool {
	classified, ok := errors.AsType[*Error](err)

	return ok && classified.Class == ClassNeedsReauth
}

func (e *Error) Error() string {
	return e.Message
}

// Unwrap exposes the underlying cause to errors.Is and errors.As.
func (e *Error) Unwrap() error {
	return e.Cause
}
