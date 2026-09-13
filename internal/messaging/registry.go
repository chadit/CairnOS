package messaging

import (
	"context"
	"slices"
	"strings"
	"sync"
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

// Registry holds the senders serve built, keyed by identity name. It is created
// empty and populated as each transport connects, so the runner and the notify
// fanout can hold it before any bridge is up. Safe for concurrent use: sends
// read it while a bridge reconnect may still be adding to it.
type Registry struct {
	byName map[string]Sender
	mu     sync.RWMutex
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Sender)}
}

// Add registers a sender under an identity name, replacing any earlier one so a
// transport that rebuilds its client after a reconnect does not leave a dead
// sender in place.
func (r *Registry) Add(name string, sender Sender) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.byName[name] = sender
}

// Get looks up one sender by identity name.
func (r *Registry) Get(name string) (Sender, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	sender, ok := r.byName[name]

	return sender, ok
}

// List describes every registered identity, sorted by name so a CLI listing and
// an API response do not reshuffle between calls over a map.
func (r *Registry) List() []Identity {
	r.mu.RLock()
	defer r.mu.RUnlock()

	identities := make([]Identity, 0, len(r.byName))
	for _, sender := range r.byName {
		identities = append(identities, sender.Describe())
	}

	slices.SortFunc(identities, func(a, b Identity) int {
		return strings.Compare(a.Name, b.Name)
	})

	return identities
}
