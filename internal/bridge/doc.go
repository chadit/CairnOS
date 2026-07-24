// Package bridge runs the Telegram side of the Phase 9b messaging bridge: a
// long-poll loop over the Bot API getUpdates method that turns allowlisted
// Telegram messages into chat turns and streams the replies back
// (.agents/spec/parity/messaging-bridge.md). It reuses the shipped chat agent
// and the store's chat sessions; it does not fork chat logic.
//
// # Trust model
//
// The bridge is an in-process trusted surface. There is no per-request token
// carrying scopes the way the gRPC API has, so authorization is expressed as
// operator-set config rather than claims:
//
//   - Access is an allowlist. Only Telegram user IDs listed under
//     [bridge.telegram.users] are answered; every other sender is dropped with
//     a debug log and counted unauthorized, never replied to.
//   - Each allowlisted user maps to a workspace. The bridge stamps that
//     workspace onto the context of every turn with store.ContextWithWorkspace
//     before it touches the store or the agent, exactly as the gRPC interceptor
//     stamps a verified token's workspace. A chat bound in one tenant is
//     invisible from another.
//   - Approving a D9 workflow proposal is a distinct grant, allow_confirm, set
//     per user. Chat access does not imply the right to fire a workflow: a
//     chat-only user's Approve tap is refused, and so is any tap from a user
//     other than the session opener. Denying is always safe and needs no grant.
//     The confirm slice (confirm.go) enforces the tap-time checks; the stance is
//     recorded here so the split stays visible from the package doc.
//
// # Token hygiene and the base URL
//
// The bot token rides CAIRNOS_BRIDGE_TELEGRAM_TOKEN, env only, and is embedded
// in every Bot API URL path (/bot<token>/method). net/http returns a *url.Error
// carrying that full URL, so a raw transport error would leak the token into
// logs and error strings. The client never returns or logs a raw transport
// error: every failure is wrapped with the method name and a token-redacted
// cause, and the same redaction runs on every log field the client and the poll
// loop emit, including the backoff and up/down lines.
//
// The Bot API base URL is fixed to DefaultBaseURL (https://api.telegram.org) on
// the production path. New builds the client through NewClient(token,
// DefaultBaseURL, ...) whenever Config.API is nil, and nothing in the serve
// wiring sets Config.API, so a running bridge always talks to the real host. The
// base URL is a parameter of NewClient only so the client's own tests can point
// it at an httptest server, and Config.API is the seam a bridge test injects a
// recorder through. Neither is ever read from a config file or an env var. That
// keeps the token-exfiltration boundary intact: a configurable base URL over a
// token-in-path API would let an operator or attacker redirect the next call,
// and the token with it, to another host. The redirect stays a compile-time test
// seam, never operator input. Any later change adding a base_url config key must
// fail review against this recorded rationale.
package bridge
