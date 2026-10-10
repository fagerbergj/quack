package tools

import (
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

const (
	// cacheTTL: short enough that a stale bot-wall response doesn't outlive a research session.
	cacheTTL     = 10 * time.Minute
	cacheMaxSize = 500
)

// URLCache: tool responses keyed by URL, shared server-wide; callers prefix keys per use.
type URLCache = expirable.LRU[string, string]

// NewURLCache's expiry goroutine never exits (golang-lru v2.0.7), so build one per server, not per tool.
func NewURLCache() *URLCache { return expirable.NewLRU[string, string](cacheMaxSize, nil, cacheTTL) }
