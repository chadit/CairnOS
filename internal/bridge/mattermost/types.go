package mattermost

import (
	"encoding/json"
	"fmt"
)

// Channel type constants. Mattermost publishes these only in server source
// (server/public/model/channel.go), not in the REST reference, so they are
// named here rather than compared as bare letters at each call site.
const (
	// ChannelOpen is a public channel.
	ChannelOpen = "O"
	// ChannelPrivate is a private channel.
	ChannelPrivate = "P"
	// ChannelDirect is a one-to-one DM.
	ChannelDirect = "D"
	// ChannelGroup is a multi-person DM, which the gate treats as a channel
	// because more than one person reads it.
	ChannelGroup = "G"
)

// EventPosted is the only WebSocket event the bridge acts on.
const EventPosted = "posted"

// Post is the part of a Mattermost post the bridge reads. The server sends
// more fields (file ids, metadata, reactions); adding one here is a decode
// change, not a protocol change, because the decoder ignores what it lacks.
type Post struct {
	// Props holds the string-valued props only. The gate reads the from_bot
	// family and activate_ai; every other prop is decoration.
	Props     map[string]string
	ID        string
	UserID    string
	ChannelID string
	RootID    string
	Message   string
	// Type is empty for a post a person wrote and non-empty for a system post
	// such as a join or a channel rename, which the gate drops.
	Type     string
	CreateAt int64
}

// postWire mirrors the server's JSON. props is heterogeneous on the wire
// (attachments and priority nest objects under it), so it decodes as raw
// messages and the string-valued entries are kept.
type postWire struct {
	Props     map[string]json.RawMessage `json:"props"`
	ID        string                     `json:"id"`
	UserID    string                     `json:"user_id"`
	ChannelID string                     `json:"channel_id"`
	RootID    string                     `json:"root_id"`
	Message   string                     `json:"message"`
	Type      string                     `json:"type"`
	CreateAt  int64                      `json:"create_at"`
}

// UnmarshalJSON decodes a post, dropping props whose value is not a string. A
// plain map[string]string would fail the whole post over an attachments array
// the gate never reads, and a dropped post is a missed turn.
func (p *Post) UnmarshalJSON(data []byte) error {
	var wire postWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("mattermost: decode post: %w", err)
	}

	*p = Post{
		ID:        wire.ID,
		UserID:    wire.UserID,
		ChannelID: wire.ChannelID,
		RootID:    wire.RootID,
		Message:   wire.Message,
		Type:      wire.Type,
		Props:     stringProps(wire.Props),
		CreateAt:  wire.CreateAt,
	}

	return nil
}

// stringProps keeps the props that decode as JSON strings and drops the rest.
func stringProps(raw map[string]json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}

	props := make(map[string]string, len(raw))

	for key, value := range raw {
		var text string
		if err := json.Unmarshal(value, &text); err == nil {
			props[key] = text
		}
	}

	if len(props) == 0 {
		return nil
	}

	return props
}

// Channel is a Mattermost channel as the bridge needs it: an id to post into,
// a name to resolve targets by, and a type the gate branches on.
type Channel struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	TeamID string `json:"team_id"`
}

// User is a Mattermost user. IsBot is the field the gate cares about: it is the
// only provenance signal available when a bot posts without the from_bot prop.
type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	IsBot    bool   `json:"is_bot"`
}

// Event is one parsed WebSocket event. Only posted events are emitted, so Post
// is always set on an Event that reaches a caller.
type Event struct {
	Post        *Post
	Name        string
	ChannelType string
	SenderName  string
	// Mentions holds the mention list as this connection sees it, which is
	// either the connection's own user id or nothing at all.
	Mentions []string
}
