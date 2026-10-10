package memory

import (
	"context"
	"fmt"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/inference"
)

// Store-backend kinds. Empty defaults to Qdrant so existing config needn't name a kind.
const (
	KindQdrant = "qdrant"
	KindSQLite = "sqlite"
)

// New opens the store adapter for kind (default qdrant): qdrant takes addr as host:port, sqlite as a
// file path (the no-docker path).
func New(ctx context.Context, kind, addr string, embedder inference.Embedder, consolidator model.LLM, collection, domain string, topK int, minScore float32) (*Store, error) {
	if kind == "" {
		kind = KindQdrant
	}
	switch kind {
	case KindQdrant:
		return Open(ctx, addr, embedder, consolidator, collection, domain, topK, minScore)
	case KindSQLite:
		return OpenSQLite(ctx, addr, embedder, consolidator, collection, domain, topK, minScore)
	default:
		return nil, fmt.Errorf("memory: unknown store kind %q", kind)
	}
}
