package memory

import (
	"context"
	"fmt"

	"google.golang.org/adk/model"

	"github.com/fagerbergj/quack/internal/inference"
)

// Store-backend kinds. Empty kind defaults to the only bundled adapter (Qdrant),
// so existing config keeps working without naming a kind. Mirrors the
// inference.NewModel provider-kind factory and the tools backend factory — the
// one selection convention for every swappable backend.
const KindQdrant = "qdrant"

// New selects the semantic-memory store adapter for kind (default: qdrant) and
// opens it. The vector store is generic OSS infra directed by addr (a URL), so it
// is bundled like searxng/crawl4ai; a future backend (e.g. pgvector) is a new
// case here, not a change to any caller.
func New(ctx context.Context, kind, addr string, embedder inference.Embedder, consolidator model.LLM, collection, domain string, topK int, minScore float32) (*Store, error) {
	if kind == "" {
		kind = KindQdrant
	}
	switch kind {
	case KindQdrant:
		return Open(ctx, addr, embedder, consolidator, collection, domain, topK, minScore)
	default:
		return nil, fmt.Errorf("memory: unknown store kind %q", kind)
	}
}
