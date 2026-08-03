// Package inference builds ADK model.LLM instances from provider config.
// It is the only place the concrete model-provider adapters are imported, so
// adding a provider kind later is localized here.
package inference

import (
	"context"
	"fmt"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/inference/openaimodel"
	"github.com/fagerbergj/quack/internal/replay"
)

// NewModel constructs an ADK model for the given provider and model name.
// Two kinds are implemented: "openai" (any OpenAI-compatible endpoint, via
// the vendored openaimodel adapter; the endpoint picks the actual server)
// and "replay" (p.Bundle, a recording - internal/replay), for hermetic
// replay of a recorded session with no live network. Both are wrapped in
// tracedModel (traced.go) so quack.model.call.duration is recorded for every
// model built anywhere in the system - the one factory, the one place to
// hook it.
func NewModel(p config.ProviderConfig, modelName string) (model.LLM, error) {
	switch p.Kind {
	case "openai":
		return &tracedModel{LLM: openaimodel.NewOpenAIModel(modelName, p.Endpoint, p.APIKey), name: modelName}, nil
	case "replay":
		sess, err := replay.Load(p.Bundle)
		if err != nil {
			return nil, fmt.Errorf("inference: replay provider: %w", err)
		}
		return NewReplayModel(sess, modelName), nil
	default:
		return nil, fmt.Errorf("inference: unsupported provider kind %q", p.Kind)
	}
}

// Embedder turns text into embedding vectors. It is a distinct capability from
// model.LLM (a different endpoint), used by the semantic-memory layer (M6).
type Embedder interface {
	// Embed returns one vector per input text, in input order.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// NewEmbedder constructs an Embedder for the given provider and embedding model.
// It reuses NewModel's provider switch - the openai adapter serves /embeddings
// too, so the same model.LLM doubles as an Embedder.
func NewEmbedder(p config.ProviderConfig, modelName string) (Embedder, error) {
	m, err := NewModel(p, modelName)
	if err != nil {
		return nil, err
	}
	return m.(Embedder), nil
}
