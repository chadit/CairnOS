package bridgetest

import "errors"

// The sentinels the fakes return. They sit in their own file so a suite can find
// what a fake can fail with without reading the fixture builders.
var (
	// ErrTurnFailed is what Chat returns for the message FailOn names.
	ErrTurnFailed = errors.New("bridgetest: turn failed")

	// ErrNoSession is what Chat returns for an unknown session id.
	ErrNoSession = errors.New("bridgetest: session not found")

	// ErrSend is what Out returns once FailSends is set.
	ErrSend = errors.New("bridgetest: send failed")
)
