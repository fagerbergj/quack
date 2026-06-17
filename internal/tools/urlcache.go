package tools

import (
	"sync"
	"time"
)

const (
	// cacheTTL is how long a fetched page stays fresh. Short enough to
	// prevent stale bot-wall responses from persisting across sessions;
	// long enough to benefit repeated fetches within one research session.
	cacheTTL = 10 * time.Minute

	// cacheMaxSize caps the number of entries to prevent unbounded heap
	// growth on long-running servers.
	cacheMaxSize = 500
)

type cacheEntry struct {
	value   string
	expires time.Time
}

// URLCache is a thread-safe in-process cache of tool responses keyed by URL,
// with TTL expiry and a size cap, so web_fetch and web_search skip redundant
// network requests within a session. Expired entries are evicted lazily on Get
// and, when the cap is reached, on Set.
type URLCache struct {
	mu    sync.Mutex
	items map[string]cacheEntry
}

// NewURLCache returns an empty cache.
func NewURLCache() *URLCache {
	return &URLCache{items: make(map[string]cacheEntry)}
}

// Get returns the cached value for key, or ("", false) if not present or expired.
func (c *URLCache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.items[key]
	if !ok {
		return "", false
	}
	if time.Now().After(entry.expires) {
		delete(c.items, key)
		return "", false
	}
	return entry.value, true
}

// Set stores value for key with a fresh TTL.
func (c *URLCache) Set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, exists := c.items[key]
	if !exists && len(c.items) >= cacheMaxSize {
		// Evict one expired entry to make room; if none are expired, skip caching.
		now := time.Now()
		for k, e := range c.items {
			if now.After(e.expires) {
				delete(c.items, k)
				break
			}
		}
		if len(c.items) >= cacheMaxSize {
			return
		}
	}
	c.items[key] = cacheEntry{value: value, expires: time.Now().Add(cacheTTL)}
}
