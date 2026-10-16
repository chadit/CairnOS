// Package registry provides the concurrency-safe name-to-value table the
// runner and messaging registries embed, so the locking lives in one place and
// each registry adds only the behavior that is its own.
package registry

import (
	"maps"
	"slices"
	"sync"
)

// Table is a name-to-value map that is safe for concurrent use. The zero value
// is an empty table ready to use, so a registry can embed it without a
// constructor.
type Table[T any] struct {
	entries map[string]T
	mu      sync.RWMutex
}

// Add stores value under name, replacing any earlier value, so a caller that
// rebuilds a value after a reconnect never leaves a stale one in place.
func (t *Table[T]) Add(name string, value T) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.entries == nil {
		t.entries = make(map[string]T)
	}

	t.entries[name] = value
}

// Get returns the value stored under name, and false when there is none.
func (t *Table[T]) Get(name string) (T, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	value, ok := t.entries[name]

	return value, ok
}

// List returns every stored name, sorted so a listing does not reshuffle
// between calls the way ranging over a map does.
func (t *Table[T]) List() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return slices.Sorted(maps.Keys(t.entries))
}

// Snapshot returns a copy of the table. A caller ranges over the copy without
// holding the lock, so work done per value, such as closing it, never blocks
// an Add or a Get.
func (t *Table[T]) Snapshot() map[string]T {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return maps.Clone(t.entries)
}
