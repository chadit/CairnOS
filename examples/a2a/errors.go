package main

import "errors"

// Static sentinel errors so the example obeys the repo's err113 and
// gochecknoglobals rules (Err* sentinels live in errors.go).
var (
	errNoToken  = errors.New("login returned no token")
	errNoTaskID = errors.New("create returned no task id")
	errStatus   = errors.New("unexpected HTTP status")
)
