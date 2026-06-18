package mcp

import "errors"

// Sentinel errors for the MCP server wiring. Construction-time failures
// (missing seams) surface as these; per-call auth failures travel as the
// canonical auth.* codes in auth.go instead.
var (
	// ErrNoManager reports a server built without a run manager, which the
	// run_workflow tool needs to start runs.
	ErrNoManager = errors.New("mcp: run manager is required")

	// ErrNoStore reports a server built without a run store, which the runs
	// resource and run_workflow's read-back need.
	ErrNoStore = errors.New("mcp: run store is required")

	// ErrNoMemory reports a server built without a memory store, which the
	// query_memory tool needs.
	ErrNoMemory = errors.New("mcp: memory store is required")

	// ErrNoResolver reports a server built without a workflow resolver, which
	// run_workflow needs to turn a name into a flow.
	ErrNoResolver = errors.New("mcp: workflow resolver is required")

	// ErrNoRegistryURL reports a registry client built without a base URL. The
	// publish and import commands gate on the configured registry URL before
	// constructing one, so this only fires on misuse.
	ErrNoRegistryURL = errors.New("mcp: registry url is required")
)
