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
)
