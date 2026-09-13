package bridgetest

import (
	"context"
	"sync"

	"github.com/chadit/CairnOS/internal/store"
)

// Store is an in-memory store.BridgeSessionStore keyed by workspace, so the
// workspace-stamping and rebinding assertions can read exactly what the core
// bound and looked up.
type Store struct {
	bindings map[string]string
	bound    []string
	mu       sync.Mutex
}

// NewStore builds an empty binding store.
func NewStore() *Store {
	return &Store{bindings: map[string]string{}}
}

// bindingKey namespaces a binding by workspace so tenants stay isolated, the way
// the real store's workspace column does.
func bindingKey(workspace, transport, chatKey string) string {
	return workspace + "|" + transport + "|" + chatKey
}

// BindBridgeSession upserts the workspace-scoped binding.
func (s *Store) BindBridgeSession(ctx context.Context, transport, chatKey, sessionID string) error {
	workspace := store.WorkspaceFromContext(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.bindings[bindingKey(workspace, transport, chatKey)] = sessionID
	s.bound = append(s.bound, workspace)

	return nil
}

// LookupBridgeSession reads the workspace-scoped binding or reports the
// not-found sentinel.
func (s *Store) LookupBridgeSession(ctx context.Context, transport, chatKey string) (string, error) {
	workspace := store.WorkspaceFromContext(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()

	sessionID, ok := s.bindings[bindingKey(workspace, transport, chatKey)]
	if !ok {
		return "", store.ErrBridgeSessionNotFound
	}

	return sessionID, nil
}

// BoundWorkspaces returns the workspaces the core bound under, in order.
func (s *Store) BoundWorkspaces() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.bound...)
}

// Binding reads the session bound under one workspace, transport, and chat key,
// so a test proves the address's own transport reached the store rather than a
// constant the core picked.
func (s *Store) Binding(workspace, transport, chatKey string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sessionID, ok := s.bindings[bindingKey(workspace, transport, chatKey)]

	return sessionID, ok
}
