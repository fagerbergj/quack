// Package inference builds ADK model.LLM instances from provider config; the only importer of provider adapters.
package inference

import (
	"context"
	"fmt"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/inference/openaimodel"
)

// NewModel wraps the provider adapter in hydratingModel, then tracedModel. effort is the default
// reasoning effort; a request's own ThinkingConfig wins.
func NewModel(p config.ProviderConfig, modelName string, artifacts artifact.Service, cost *config.ModelPricing, effort string) (model.LLM, error) {
	switch p.Kind {
	case "openai":
		live := &hydratingModel{LLM: openaimodel.NewOpenAIModel(modelName, p.Endpoint, p.APIKey, effort), artifacts: artifacts}
		tm := &tracedModel{LLM: live, name: modelName}
		tm.pricing = cost
		return tm, nil
	default:
		return nil, fmt.Errorf("inference: unsupported provider kind %q", p.Kind)
	}
}

// Embedder is a distinct capability from model.LLM (a different endpoint).
type Embedder interface {
	// Embed returns one vector per input text, in input order.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// NewEmbedder reuses NewModel without artifacts: Embed never sees attachments.
func NewEmbedder(p config.ProviderConfig, modelName string, cost *config.ModelPricing) (Embedder, error) {
	m, err := NewModel(p, modelName, nil, cost, "")
	if err != nil {
		return nil, err
	}
	return m.(Embedder), nil
}
