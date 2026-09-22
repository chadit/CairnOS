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
