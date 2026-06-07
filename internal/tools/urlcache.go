package tools

import (
	"context"
	"sync"
	"time"
)

// URLCache caches tool responses by key to avoid redundant network requests.
// The interface is intentionally minimal so an in-memory implementation can be
// swapped for a persistent (DB-backed) one without changing callers.
type URLCache interface {
	// Get returns the cached value for key, or ("", false) if not present or expired.
	Get(ctx context.Context, key string) (value string, ok bool)
	// Set stores value for key. sessionID and appName are metadata that
	// identify the session that populated the entry — reserved for future
	// audit and reverse-lookup in a persistent backend.
	Set(ctx context.Context, key, value, sessionID, appName string)
}

const (
	// cacheTTL is how long a fetched page stays fresh. Short enough to
	// prevent stale bot-wall responses from persisting across sessions;
	// long enough to benefit repeated fetches within one research session.
	cacheTTL = 10 * time.Minute

	// cacheMaxSize caps the number of entries in the in-process cache to
	// prevent unbounded heap growth on long-running servers.
	cacheMaxSize = 500
)

type cacheEntry struct {
	value   string
	expires time.Time
}

// inMemoryURLCache is a thread-safe in-process URLCache with TTL expiry and a
// size cap. Expired entries are evicted lazily on Get and, when the cap is
// reached, on Set.
type inMemoryURLCache struct {
	mu    sync.Mutex
	items map[string]cacheEntry
}

// NewInMemoryURLCache returns a URLCache backed by a plain Go map.
func NewInMemoryURLCache() URLCache {
	return &inMemoryURLCache{items: make(map[string]cacheEntry)}
}

func (c *inMemoryURLCache) Get(_ context.Context, key string) (string, bool) {
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

func (c *inMemoryURLCache) Set(_ context.Context, key, value, _, _ string) {
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
