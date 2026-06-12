package reliability

import "errors"

// Sentinel errors for the reliability package.
var (
	// ErrCircuitOpen reports a call rejected without touching the service
	// because the runner's breaker is OPEN, or its single HALF_OPEN probe
	// slot is already taken. The engine surfaces it under the canonical
	// engine.circuit_open code.
	ErrCircuitOpen = errors.New("engine.circuit_open: circuit breaker is open")
)
