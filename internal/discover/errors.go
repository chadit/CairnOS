package discover

import "errors"

// Errors CheckSchedulable returns for a workflow no scheduled fire could run.
var (
	ErrNoSchedule       = errors.New("has no schedule key")
	ErrUnsuppliedInputs = errors.New("required input(s) with no default that no scheduled fire supplies")
)
