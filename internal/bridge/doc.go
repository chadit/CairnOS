// Package bridge is the transport-neutral half of the messaging bridge: it binds
// a conversation to a chat session, runs one worker per conversation, folds a
// turn's events into a reply, and enforces the D9 confirmation gate. It reuses
// the shipped chat agent and the store's chat sessions; it does not fork chat
// logic.
//
// A transport adapter owns its wire: the connection loop, the wire types, the
// rendering, and whatever allowlist or gate decides who may talk to the agent.
// It hands the core an Inbound and receives replies back through that inbound's
// Outbound, so nothing here knows what a Telegram callback or a Mattermost post
// looks like. The shipped adapters live in subpackages, starting with
// bridge/telegram.
//
// # Trust model
//
// The bridge is an in-process trusted surface. There is no per-request token
// carrying scopes the way the gRPC API has, so authorization is expressed as
// operator-set config rather than claims. The adapter resolves it and the core
// enforces it:
//
//   - Access is the adapter's decision. A sender the adapter does not recognize
//     never becomes an Inbound: it is dropped with a debug log and counted
//     unauthorized, never replied to.
//   - Each recognized sender maps to a Grant naming a workspace. The core stamps
//     that workspace onto the context of every turn with
//     store.ContextWithWorkspace before it touches the store or the agent,
//     exactly as the gRPC interceptor stamps a verified token's workspace. A
//     conversation bound in one tenant is invisible from another.
//   - Approving a D9 workflow proposal is a distinct grant, AllowConfirm. Chat
//     access does not imply the right to fire a workflow: an approve from a
//     sender without the grant is refused, and so is any decision from someone
//     other than the session opener. Denying is always safe and needs no grant.
//     The confirm slice (confirm.go) enforces both checks for every transport,
//     which is why they are not repeated in any adapter.
package bridge
