package config

import "errors"

// ErrInvalidConfig reports a config file or environment override that failed
// to parse. The CLI surfaces it as the canonical config.invalid error.
var ErrInvalidConfig = errors.New("invalid configuration")

// Shapes a host path is refused for, kept as sentinels so each caller names the
// key it checked while tests match the rule with errors.Is.
var (
	ErrHostPathRelative      = errors.New(`must be absolute or start with "~/"`)
	ErrHostPathTrailingSlash = errors.New("must not end with a slash")
	ErrHostPathPattern       = errors.New("must match")
)

// Sentinels for the allow_actions editor. Each caller wraps one with the detail
// the operator needs; tests match the rule with errors.Is.
var (
	// ErrNoExecTable reports a config with no [runner.exec] table. The editor
	// refuses rather than creating one, because a written exec block must carry
	// a timeout and serve would then refuse to start without it.
	ErrNoExecTable = errors.New("no [runner.exec] table")

	// ErrAllowActionsShape reports an allow_actions array, or a [runner.exec]
	// table, written in a form the text editor does not rewrite.
	ErrAllowActionsShape = errors.New("allow_actions is in a shape this command does not edit")

	// ErrAllowActionsVerify reports an edit whose re-parse disagrees with the
	// expected result. Nothing is written when it fires.
	ErrAllowActionsVerify = errors.New("the edited config did not decode to the expected allow_actions")
)

// ErrCredentialsFileVerify reports a credentials_file edit whose re-parse is not
// the original document with that one key set. Nothing is written when it fires.
var ErrCredentialsFileVerify = errors.New("the edited config did not decode to the expected " +
	"[secrets.bitwarden] credentials_file")

// errGlob* are the reasons validateWorkflowGlob gives, phrased to follow the
// quoted glob in the load error.
var (
	errGlobEmpty    = errors.New("must not be empty")
	errGlobAbsolute = errors.New("must be relative to dest")
	errGlobParent   = errors.New("must not contain a .. element")
	errGlobDot      = errors.New("must not contain a . element")
	errGlobSlashes  = errors.New("must not contain an empty element or end in a slash")
)
