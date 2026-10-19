package nettest

import "errors"

// errNoListener marks a dial for an address a Network never handed out,
// which means the fixture is wired wrong.
var errNoListener = errors.New("no in-memory listener")
