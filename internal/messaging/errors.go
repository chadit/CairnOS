package messaging

import "errors"

// Sentinel errors for the outbound send path.
var (
	// ErrTargetUnrecognized reports a target that is not one of the four
	// accepted forms. The service turns it into InvalidArgument.
	ErrTargetUnrecognized = errors.New("target must be #channel-name, @username, channel:<id>, or user:<id>")

	// ErrThreadNotFound reports a reply whose thread root the chat server no
	// longer has, so a caller can open a fresh thread instead of failing.
	ErrThreadNotFound = errors.New("thread root not found")
)
