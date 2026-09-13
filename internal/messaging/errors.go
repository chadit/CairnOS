package messaging

import "errors"

// Sentinel errors for the outbound send path.
var (
	// ErrTargetUnrecognized reports a target that is not one of the four
	// accepted forms. The service turns it into InvalidArgument (FR27).
	ErrTargetUnrecognized = errors.New("target must be #channel-name, @username, channel:<id>, or user:<id>")
)
