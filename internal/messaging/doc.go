// Package messaging is the outbound send path: one registry of named
// identities, each able to post to a chat system, behind the MessagingService
// gRPC surface and its JSON gateway binding (FR26-FR30).
//
// The package owns no transport. A transport package (the Mattermost bridge,
// today) builds a Sender and adds it to the Registry under the identity name
// from config, so serve wiring is the only place that knows both halves and
// this package never imports a chat client. That is also why the registry is
// created empty and populated later: the message runner and the notify fanout
// hold it before any bridge has connected.
//
// Targets arrive as text from a CLI flag, a workflow param, or a JSON body, so
// ParseTarget is deliberately strict about the four accepted forms. A bare id
// is refused rather than guessed at, because "general" is a plausible channel
// name and a plausible channel id, and posting to the wrong one is not
// recoverable.
package messaging
