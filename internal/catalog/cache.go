package catalog

import (
	"sync"
	"time"
)

// How long each kind of answer is kept.
//
// Short, and different per kind, because the three go stale at different
// rates: a search index moves constantly, a project's description almost
// never, and a version list only when its author publishes. The point is not
// to be clever about freshness but to stop the panel asking Modrinth the same
// question on every keystroke — their guidelines ask for exactly that, and a
// panel that hammers a free API gets everyone using it rate-limited.
const (
	searchTTL   = 5 * time.Minute
	projectTTL  = 30 * time.Minute
	versionsTTL = 10 * time.Minute
)

// maxEntries bounds the cache. A search box generates a new key per keystroke,
// so without a bound this grows for as long as the daemon runs.
const maxEntries = 500

type entry struct {
	value     any
	expiresAt time.Time
}

// cache is a small TTL map.
//
// Deliberately not an LRU: the access pattern here is a handful of repeated
// lookups within minutes, so evicting whatever has expired — and, when that is
// not enough, clearing the lot — costs nothing anyone will notice and is a
// fraction of the code.
type cache struct {
	mu      sync.Mutex
	entries map[string]entry
}

func newCache() *cache { return &cache{entries: make(map[string]entry)} }

func (c *cache) get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	found, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(found.expiresAt) {
		delete(c.entries, key)
		return nil, false
	}
	return found.value, true
}

func (c *cache) set(key string, value any, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.entries) >= maxEntries {
		c.evictLocked()
	}
	c.entries[key] = entry{value: value, expiresAt: time.Now().Add(ttl)}
}

// evictLocked drops expired entries, and everything if that frees nothing.
func (c *cache) evictLocked() {
	now := time.Now()
	for key, found := range c.entries {
		if now.After(found.expiresAt) {
			delete(c.entries, key)
		}
	}
	if len(c.entries) >= maxEntries {
		c.entries = make(map[string]entry)
	}
}

// Len reports how many entries are held, for tests.
func (c *cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
