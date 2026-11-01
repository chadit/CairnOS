// Package mcp exposes a subset of CairnOS as a Model Context Protocol server,
// so an external MCP client (Claude, ChatGPT, others) discovers and calls
// CairnOS with no custom glue. It is a protocol surface, not a second
// implementation: every tool and resource reuses the run manager, the engine,
// the memory store, the workflow loader, and the run store the rest of the API
// already owns.
//
// The surface shipped at Phase 7a:
//
//   - tool run_workflow: start a run by name with inputs, backed by the run
//     manager (scope write:workflow).
//   - tool query_memory: search memory, backed by memory.Store.Search (scope
//     read:memory).
//   - resource workflows: list the workflow files in the configured directory
//     with their declared inputs (scope read:workflow).
//   - resource runs: recent run state from the store (scope read:run).
//
// Auth maps onto MCP the same way it maps onto gRPC: a caller presents the same
// bearer token and CairnOS enforces the same scopes. The token rides the HTTP
// transport's Authorization header; the stdio transport has no header, so there
// auth is the process boundary (the operator who can spawn the process already
// holds the machine). When the configured authenticator is nil, auth is
// disabled and every call passes, matching the rest of the API.
//
// The get_language_rules tool landed in Phase 7b once the rules service
// existed; it registers only when a rules store is wired (see server.go). The
// MCP registry publish/import is the lowest-priority piece of spec 10 and stays
// deferred (see registry.go).
package mcp
