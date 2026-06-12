package mcp

// MCP Registry publish/import is deferred.
//
// Spec 10 (.agents/spec/10-mcp-integration.md, "MCP Registry") lets a node
// publish its MCP tools to the public registry and import community-registered
// tools as mcp_client runners. The spec calls it the lowest-priority piece of
// Phase 7 and explicitly fine to defer, so 7a ships the server surface (tools
// and resources) without the registry.
//
// The extension point when it lands:
//
//   - Publish: register this server's tools (the run_workflow and query_memory
//     Tool declarations from server.go) with a registry client. Since one
//     Service already knows the full tool surface, a Publish method here would
//     enumerate it and POST to the registry endpoint from config.
//   - Import: pull registry entries and turn each into an mcp_client runner
//     config block, which the existing builtins.MCPClient already executes
//     (internal/runner/builtins/mcp_client.go). Import is therefore a config
//     generation step, not new runtime code.
//
// No type is declared here on purpose: a stub interface with no implementation
// would be dead surface. This file is the documented placeholder so the next
// phase knows where the registry wiring belongs.
