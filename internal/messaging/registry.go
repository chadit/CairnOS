package messaging

import (
	"context"
	"slices"
	"strings"

	"github.com/chadit/CairnOS/internal/registry"
)

// Identity describes one configured send surface: the name a caller addresses
// it by, the chat system behind it, and the account a recipient sees. The URL
// is the server the identity posts to, so an operator with two Mattermost
// servers can tell two same-named bots apart.
type Identity struct {
	Name        string
	Transport   string
	Username    string
	DisplayName string
	URL         string
}

// PostRef identifies a created post, so a caller can root a follow-up in the
// same thread without a second lookup.
type PostRef struct {
	PostID    string
	ChannelID string
}

// Sender is one identity's send surface. A transport package implements it and
// registers the result; this package never learns which chat system is behind
// it. Send resolves the target itself, because name-to-id resolution is
// transport-specific and needs the transport's own cache and credentials.
type Sender interface {
	Send(ctx context.Context, to Target, text, threadID string) (PostRef, error)
	Describe() Identity
}

// Registry holds the senders serve built, keyed by identity name, and is safe
// for concurrent use. It starts empty and fills as each transport connects, so
// the runner and the notify fanout can hold it before any bridge is up. Add
// replaces an earlier sender under the same name, so a reconnecting transport
// leaves no dead sender behind.
type Registry struct {
	registry.Table[Sender]
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// List describes every registered identity, sorted by name so a CLI listing and
// an API response do not reshuffle between calls over a map.
func (r *Registry) List() []Identity {
	senders := r.Snapshot()

	identities := make([]Identity, 0, len(senders))
	for _, sender := range senders {
		identities = append(identities, sender.Describe())
	}

	slices.SortFunc(identities, func(a, b Identity) int {
		return strings.Compare(a.Name, b.Name)
	})

	return identities
}
