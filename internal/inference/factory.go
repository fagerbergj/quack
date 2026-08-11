// Package inference builds ADK model.LLM instances from provider config.
// It is the only place the concrete model-provider adapters are imported, so
// adding a provider kind later is localized here.
package inference

import (
	"context"
	"fmt"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/inference/openaimodel"
	"github.com/fagerbergj/quack/internal/replay"
)

// NewModel constructs an ADK model for the given provider and model name.
// "openai" is wrapped in hydratingModel (hydrate.go) then tracedModel
// (traced.go) - the one factory, the one place to hook both.
func NewModel(p config.ProviderConfig, modelName string, artifacts artifact.Service) (model.LLM, error) {
	switch p.Kind {
	case "openai":
		live := &hydratingModel{LLM: openaimodel.NewOpenAIModel(modelName, p.Endpoint, p.APIKey), artifacts: artifacts}
		return &tracedModel{LLM: live, name: modelName}, nil
	case "replay":
		sess, err := replay.Load(p.Bundle)
		if err != nil {
			return nil, fmt.Errorf("inference: replay provider: %w", err)
		}
		if p.ForkMode != "fork" {
			return NewReplayModel(sess, modelName), nil
		}
		// Fork-replay (#605): p.Live is the caller's REAL provider config,
		// built through this SAME factory - wrapped like any other model.
		sess.EnableFork(p.ForkFrom)
		live, err := NewModel(*p.Live, modelName, artifacts)
		if err != nil {
			return nil, fmt.Errorf("inference: replay provider: live delegate: %w", err)
		}
		return NewReplayModelFork(sess, modelName, live), nil
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

// NewEmbedder constructs an Embedder for the given provider and embedding
// model, reusing NewModel's switch. artifacts is only for signature
// symmetry with NewModel - Embed never sees attachment parts.
func NewEmbedder(p config.ProviderConfig, modelName string, artifacts artifact.Service) (Embedder, error) {
	m, err := NewModel(p, modelName, artifacts)
	if err != nil {
		return nil, err
	}
	return m.(Embedder), nil
}
