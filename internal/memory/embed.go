// Package memory implements Quack's long-term memory (milestone M4): a
// Qdrant-backed implementation of the ADK memory.Service interface that agents
// recall through the load_memory / preload_memory tools, plus a Commit helper
// the trust gate calls to store a single vetted finding after a passing judge
// verdict. Embeddings come from an OpenAI-compatible endpoint.
//
// Scope is the ADK default: every memory is keyed by (appName, userID), and
// each agent serves under its own appName, so an agent recalls only its own
// prior vetted findings. The gate commits under the same (appName, userID) the
// recall adapter searches, so the two line up without a shared namespace.
package memory

import (
	"context"
	"fmt"
	"log"
	"time"

	goopenai "github.com/sashabaranov/go-openai"
)

// Embedder turns text into vectors. Embed returns one vector per input, in
// order. Dim is the vector dimension, resolved once at startup and used to
// create the Qdrant collection.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dim() int
}

// openAIEmbedder calls an OpenAI-compatible /v1/embeddings endpoint. It targets
// a resident embedding model (e.g. qwen3-embed) on its own endpoint, separate
// from the chat models.
type openAIEmbedder struct {
	client *goopenai.Client
	model  string
	dim    int
}

// NewOpenAIEmbedder builds an embedder against the OpenAI-compatible endpoint at
// baseURL using model. It issues one probe embedding to discover the vector
// dimension, so a misconfigured endpoint fails fast at startup rather than at
// first commit. The probe is retried because a resident model behind llama-swap
// can cold-load on first request (a transient 502 until it is warm).
func NewOpenAIEmbedder(ctx context.Context, baseURL, apiKey, model string) (Embedder, error) {
	cfg := goopenai.DefaultConfig(apiKey)
	cfg.BaseURL = baseURL
	e := &openAIEmbedder{client: goopenai.NewClientWithConfig(cfg), model: model}

	for attempt := 1; ; attempt++ {
		probe, err := e.Embed(ctx, []string{"quack memory probe"})
		if err == nil {
			if len(probe) != 1 || len(probe[0]) == 0 {
				return nil, fmt.Errorf("embeddings: probe returned no vector")
			}
			e.dim = len(probe[0])
			return e, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("embeddings: probe %q at %q: %w", model, baseURL, err)
		}
		log.Printf("embeddings: probe %q at %q attempt %d failed (model may be cold-loading): %v", model, baseURL, attempt, err)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("embeddings: probe %q at %q: %w", model, baseURL, ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
}

func (e *openAIEmbedder) Dim() int { return e.dim }

func (e *openAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	resp, err := e.client.CreateEmbeddings(ctx, goopenai.EmbeddingRequestStrings{
		Model: goopenai.EmbeddingModel(e.model),
		Input: texts,
	})
	if err != nil {
		return nil, fmt.Errorf("embeddings: create: %w", err)
	}
	if len(resp.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings: got %d vectors for %d inputs", len(resp.Data), len(texts))
	}
	out := make([][]float32, len(resp.Data))
	for _, d := range resp.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("embeddings: vector index %d out of range", d.Index)
		}
		out[d.Index] = d.Embedding
	}
	return out, nil
}
