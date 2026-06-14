package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// Summary cache — makes compaction "summarize once" instead of re-summarizing the
// whole head on every over-budget turn.
//
// The head is chunked from the START (see chunkByChars), and prior turns' events
// are immutable, so every chunk except the trailing partial one is byte-identical
// turn-over-turn. Keying summaries by chunk content hash means those stable chunks
// are summarized exactly once and reused; only the growing tail chunk is redone.
// Content-addressed, so it's correct and concurrency-safe across nodes with no
// per-session state — identical content always summarizes to an equally-valid
// briefing, and sharing one is the point.
//
// (When native Go EventCompaction lands — google/adk-go #1001 — it writes a
// compaction event into the session and supersedes this; remove then.)

// summaryCacheMax bounds the cache so a long-running server can't grow it without
// limit. A node that's over budget has only a handful of live chunks, so this
// comfortably covers concurrent nodes' active chunks while evicting stale ones.
const summaryCacheMax = 128

var summaryCache = newBoundedCache(summaryCacheMax)

// boundedCache is a tiny FIFO-evicting string→string cache, safe for concurrent use.
type boundedCache struct {
	mu   sync.Mutex
	max  int
	m    map[string]string
	keys []string // insertion order, for FIFO eviction
}

func newBoundedCache(max int) *boundedCache {
	return &boundedCache{max: max, m: make(map[string]string, max)}
}

func (c *boundedCache) get(k string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v, ok
}

func (c *boundedCache) put(k, v string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[k]; ok {
		return
	}
	if len(c.keys) >= c.max {
		oldest := c.keys[0]
		c.keys = c.keys[1:]
		delete(c.m, oldest)
	}
	c.m[k] = v
	c.keys = append(c.keys, k)
}

// chunkKey is the content-address of a chunk of summary input.
func chunkKey(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
