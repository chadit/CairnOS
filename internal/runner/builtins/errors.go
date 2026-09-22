package builtins

import "errors"

// Construction-time sentinels for runner factories. Classified runner.Error
// values cover execution failures; these cover config decoding, which happens
// before any task exists.
var (
	// ErrMessageThreadMissing marks a reply whose root post the chat server no longer
	// has. Serve wraps the transport's own sentinel in it so this package stays off messaging.
	ErrMessageThreadMissing = errors.New("message thread root is gone")

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

	// errStdoutPipe reports that a command's stdout pipe could not be opened.
	// The three process runners classify a start failure and a pipe failure
	// differently, so startPiped marks the pipe case for them.
	errStdoutPipe = errors.New("open stdout pipe")

	// errUsagePipe reports that a launcher's stdin pipe could not be opened. It
	// sits beside errStdoutPipe because the usage runner writes as well as
	// reads, so a failure on either pipe ends the exchange before it starts.
	errUsagePipe = errors.New("open stdin pipe")

	// errUsageProtocol reports a launcher configured under a protocol the usage
	// runner has no dialog for. Config decoding builds the pairs, so this stays
	// defensive.
	errUsageProtocol = errors.New("no usage dialog for protocol")

	// errUsageRefused reports a CLI that answered the usage request with a
	// failure of its own: an expired login, a plan that reports no limits, or a
	// launcher pointed at an account the vendor rejected.
	errUsageRefused = errors.New("cli refused the usage request")

	// errUsageNoLimits reports an answer that carried no rate-limit windows.
	// The login works and the plan simply publishes nothing, which is a fact
	// about that login rather than a broken probe.
	errUsageNoLimits = errors.New("cli reported no rate limit windows")

	// errUsageNoResponse reports a launcher whose stdout ended before the usage
	// answer arrived, which is what a wrapper that never reached the CLI looks
	// like from here.
	errUsageNoResponse = errors.New("cli ended without a usage response")

	// errUsageTimeout reports a probe that outlived its deadline.
	errUsageTimeout = errors.New("cli timed out")

	// errUsageCanceled reports a probe the caller ended. It is kept apart from
	// the timeout because a run someone stopped says nothing about the CLI.
	errUsageCanceled = errors.New("cli probe canceled")

	// errUsageBadWindow reports a rate-limit window the CLI sent in a shape this
	// build cannot read. It surfaces only when no window decoded at all, since
	// one unreadable key among several is a field this build has yet to learn.
	errUsageBadWindow = errors.New("cli sent an unreadable rate limit window")

	// errUsageUnprobed reports a launcher the run never reached, because the
	// wait between two Claude logins was cut short.
	errUsageUnprobed = errors.New("not probed: the run ended while spacing claude logins apart")

	// errUsageNoHome reports a "~/" launcher with no HOME to expand it against.
	errUsageNoHome = errors.New("no home directory in the environment")

	// errBadOutput reports a Starlark output value the code runner refuses to
	// hand downstream: a non-data type, an int outside int64, or a dict with a
	// non-string key. The runner wraps it as code.bad_output. Failing here
	// keeps output types stable across runs instead of letting them drift.
	errBadOutput = errors.New("output is not a json-shaped value")
)
