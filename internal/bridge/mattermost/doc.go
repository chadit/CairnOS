// Package mattermost carries the transport-independent half of the Mattermost
// bridge: a small REST and WebSocket client, a shared rate limiter, the event
// parser, and the inbound gate that decides which posts become chat turns.
//
// The client is hand-rolled over net/http rather than built on
// github.com/mattermost/mattermost/server/public. The bridge drives eight
// endpoints, the official module pulls a large dependency tree, and its go
// directive would raise this repo's toolchain floor. The cost of
// owning the wire shapes is paid down by the recorded frames in testdata.
//
// Two wire details are server implementation details rather than documented
// API, so they are pinned by fixtures:
//
//   - A "posted" event carries the post as a JSON string nested inside the
//     event's JSON object, which needs a second unmarshal.
//   - The "mentions" field is rewritten per connection. A connection sees
//     either its own user id in the list or no field at all, so a bot can
//     learn that it was mentioned but never who else was.
//
// The bearer token rides an Authorization header, never a URL path, so no
// error string or log line in this package can leak it the way a Bot API URL
// would. The client still never logs headers.
package mattermost
