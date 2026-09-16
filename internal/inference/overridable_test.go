package inference

import (
	"context"
	"iter"
	"testing"

	"google.golang.org/adk/v2/model"
)

type stubLLM struct{ name string }

func (s stubLLM) Name() string { return s.name }
func (s stubLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {}
}

func TestOverridableModelSet(t *testing.T) {
	m := NewOverridable(stubLLM{name: "base"})
	if got := m.Name(); got != "base" {
		t.Fatalf("Name() = %q, want base", got)
	}
	m.Set(stubLLM{name: "bound"})
	if got := m.Name(); got != "bound" {
		t.Fatalf("Name() = %q, want bound after Set", got)
	}
}
