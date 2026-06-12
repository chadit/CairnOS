package config

import "errors"

// ErrInvalidConfig reports a config file or environment override that failed
// to parse. The CLI surfaces it as the canonical config.invalid error.
var ErrInvalidConfig = errors.New("invalid configuration")
