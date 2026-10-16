package bitwarden

import "errors"

// errServeExited marks a child that died before it answered, which on a busy
// host means it lost the port between this process releasing it and the child
// binding it.
var errServeExited = errors.New("bw serve exited before it answered")

// errServeSilent marks a child that stayed up and never answered. A port the
// host has stopped carrying traffic for looks exactly like this, so it is the
// second failure another port can fix.
var errServeSilent = errors.New("bw serve never answered")

// errAnswered matches every statusError under errors.Is, so a caller can tell a
// call the daemon replied to from one the transport lost, whatever the code.
var errAnswered = errors.New("the bw daemon answered")
