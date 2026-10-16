// Package telegram is the Telegram adapter for the messaging bridge: a long-poll
// loop over the Bot API getUpdates method that turns allowlisted Telegram
// updates into bridge.Inbound values and renders the core's replies back as Bot
// API messages. Session binding, the turn fold, and the D9 confirmation gate all
// live in the core; this package owns only the wire.
//
// # Trust model
//
// Access is an allowlist. Only Telegram user IDs listed under
// [bridge.telegram.users] are answered; every other sender is dropped with a
// debug log and counted unauthorized, never replied to. Each allowlisted user
// maps to a bridge.Grant naming the workspace its turns are stamped with and
// whether it may approve a D9 proposal. The core enforces both, so the stance is
// recorded in the core's package doc and not repeated per check here.
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
// the production path. New builds the client through NewClientOver(token,
// DefaultBaseURL, ..., Config.Transport) whenever Config.API is nil, and nothing
// in the serve wiring sets Config.API, so a running bridge always addresses the
// real host. The base URL is a parameter of NewClient only so the client's own
// tests can point it at an httptest server. Config.API is the seam an adapter
// suite injects a recorder through, and Config.Transport is how serve's suite
// answers the poll in the process. None of them is ever read from a config file
// or an env var. That
// keeps the token-exfiltration boundary intact: a configurable base URL over a
// token-in-path API would let an operator or attacker redirect the next call,
// and the token with it, to another host. The redirect stays a compile-time test
// seam, never operator input. Any later change adding a base_url config key must
// fail review against this recorded rationale.
package telegram
