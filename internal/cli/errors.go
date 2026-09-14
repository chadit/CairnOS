package cli

import "errors"

// Sentinel errors for the CLI.
var (
	// ErrBadInput reports a malformed or uncoercible --input flag value.
	ErrBadInput = errors.New("bad --input value")

	// ErrScheduledRunFailed reports a scheduled fire whose run finished in a
	// non-succeeded status; the daemon logs it and keeps running.
	ErrScheduledRunFailed = errors.New("scheduled run failed")

	// errOAuthNeedsAuth reports an OAuth login configured with auth disabled: a
	// minted token is only useful if the interceptor verifies it.
	errOAuthNeedsAuth = errors.New("oauth requires auth")

	// errFlagValue reports a flag whose value cannot work. Catching it here beats
	// finding out from a server that started on the wrong port: --port 0 makes
	// llama-server bind an ephemeral port while every probe still targets :0, so
	// an upgrade would roll back a runtime that was running fine.
	errFlagValue = errors.New("model: flag value out of range")
)
