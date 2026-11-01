// Package openaicompat adapts CairnOS's Phase 9 ChatService to the
// OpenAI-compatible chat completions wire shape, so off-the-shelf OpenAI
// clients and chat frontends (OpenWebUI, LibreChat, the openai SDKs) can point
// at a CairnOS node with a base URL and a token. It is protocol translation
// only, not a second chat implementation: the one dangerous action, firing a
// workflow, stays on the already-guarded native surface.
//
// # What maps to what
//
//   - POST /v1/chat/completions runs one turn over ChatService.
//   - The model field names a personality. Empty uses the default; an unknown
//     name returns 404 model_not_found.
//   - Bearer auth rides the existing token store and the chat:session scope, the
//     same check the gRPC interceptor makes. A nil authenticator disables auth
//     and every call runs on the default workspace.
//   - Each request opens a fresh session (OpenAI is stateless, ChatService is
//     stateful), seeds every message but the trailing one into it, and runs the
//     trailing user message as the turn. The completion id embeds the session id
//     so a pending gate is findable from client logs.
//
// # Ignored tuning params
//
// temperature, top_p, max_tokens, n, stop, tools, tool_choice, response_format
// and the rest are accepted and ignored: the driver owns its own settings. This
// is written down so a silent param is not filed as a bug.
//
// # Streaming is one delta
//
// ChatService emits the final answer as a single assistant chunk, so an SSE
// stream carries one large delta rather than token-level streaming. Honest and
// spec-compliant; true incremental streaming needs driver-level chunking and is
// out of scope. stream:true returns text/event-stream with a role chunk, one
// content chunk per assistant piece, a stop chunk, and a terminating [DONE]; a
// mid-turn failure surfaces as an SSE error frame before [DONE].
//
// # No confirm route by design
//
// A run_workflow proposal surfaces as assistant text naming the pending
// proposal, never as an approvable action. The adapter exposes no confirm
// route, so a foreign client can never fire a workflow: approval exists only on
// the native ChatService path (ConfirmAction, which enforces write:workflow).
package openaicompat
