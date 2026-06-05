// Package inference builds ADK model.LLM instances from provider config.
// It is the only place the concrete model-provider adapters are imported, so
// adding a provider kind later is localized here.
package inference

import (
	"fmt"

	goopenai "github.com/sashabaranov/go-openai"
	"google.golang.org/adk/model"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/inference/openaimodel"
)

// NewModel constructs an ADK model for the given provider and model name.
// M0 implements only kind "openai" (any OpenAI-compatible endpoint, via the
// vendored openaimodel adapter; the endpoint picks the actual server).
func NewModel(p config.ProviderConfig, modelName string) (model.LLM, error) {
	switch p.Kind {
	case "openai":
		cfg := goopenai.DefaultConfig(p.APIKey)
		cfg.BaseURL = p.Endpoint
		return openaimodel.NewOpenAIModel(modelName, cfg), nil
	default:
		return nil, fmt.Errorf("inference: unsupported provider kind %q", p.Kind)
	}
}
