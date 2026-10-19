package mattermost

import (
	"container/list"
	"sync"
)

// userCache is a bounded LRU over user lookups. The gate needs is_bot for the
// author of every post that carries no from_bot prop, and the same few
// integration ids repeat all day, so the cache turns a per-post round trip into
// a map read. Safe for concurrent use.
type userCache struct {
	entries map[string]*list.Element
	order   *list.List
	mu      sync.Mutex
	limit   int
}

// cacheEntry keeps its own key so an eviction can find the map entry from the
// list element it drops.
type cacheEntry struct {
	key  string
	user User
}

func newUserCache(limit int) *userCache {
	return &userCache{entries: make(map[string]*list.Element, limit), order: list.New(), limit: limit}
}

// CachedUser returns the cached user and marks it most recently used.
func (c *userCache) CachedUser(id string) (User, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	element, ok := c.entries[id]
	if !ok {
		return User{}, false
	}

	c.order.MoveToFront(element)

	entry, _ := element.Value.(*cacheEntry)

	return entry.user, true
}

// put stores a user, evicting the least recently used entry once the cache is
// full.
func (c *userCache) put(id string, user User) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if element, ok := c.entries[id]; ok {
		entry, _ := element.Value.(*cacheEntry)
		entry.user = user

		c.order.MoveToFront(element)

		return
	}

	c.entries[id] = c.order.PushFront(&cacheEntry{key: id, user: user})

	if c.order.Len() > c.limit {
		c.evictOldestLocked()
	}
}

// evictOldestLocked drops the tail entry. The caller holds mu.
func (c *userCache) evictOldestLocked() {
	oldest := c.order.Back()
	if oldest == nil {
		return
	}

	c.order.Remove(oldest)

	entry, _ := oldest.Value.(*cacheEntry)
	delete(c.entries, entry.key)
}
