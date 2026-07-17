package inference

import (
	"context"
	"errors"
	"iter"
	"testing"

	"google.golang.org/adk/v2/model"
)

// stubModel is a minimal model.LLM for testing tracedModel's passthrough.
type stubModel struct {
	name  string
	resps []*model.LLMResponse
	err   error
}

func (s *stubModel) Name() string { return s.name }

func (s *stubModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for _, r := range s.resps {
			if !yield(r, nil) {
				return
			}
		}
		if s.err != nil {
			yield(nil, s.err)
		}
	}
}

// embeddableStub additionally implements Embedder.
type embeddableStub struct {
	stubModel
	vectors [][]float32
	err     error
}

func (e *embeddableStub) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if e.err != nil {
		return nil, e.err
	}
	return e.vectors, nil
}

func TestTracedModel_NamePassthrough(t *testing.T) {
	tm := &tracedModel{LLM: &stubModel{name: "qwen3-coder"}, name: "qwen3-coder"}
	if got := tm.Name(); got != "qwen3-coder" {
		t.Errorf("Name() = %q, want qwen3-coder", got)
	}
}

func TestTracedModel_GenerateContentPassesThroughAllResponses(t *testing.T) {
	want := []*model.LLMResponse{{}, {}, {}}
	tm := &tracedModel{LLM: &stubModel{name: "m", resps: want}, name: "m"}

	var got []*model.LLMResponse
	for r, err := range tm.GenerateContent(context.Background(), &model.LLMRequest{}, true) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d responses, want %d", len(got), len(want))
	}
}

func TestTracedModel_GenerateContentPassesThroughError(t *testing.T) {
	wantErr := errors.New("boom")
	tm := &tracedModel{LLM: &stubModel{name: "m", err: wantErr}, name: "m"}

	var gotErr error
	for _, err := range tm.GenerateContent(context.Background(), &model.LLMRequest{}, true) {
		if err != nil {
			gotErr = err
		}
	}
	if !errors.Is(gotErr, wantErr) {
		t.Errorf("error = %v, want %v", gotErr, wantErr)
	}
}

func TestTracedModel_GenerateContentStopsEarlyOnConsumerBreak(t *testing.T) {
	resps := []*model.LLMResponse{{}, {}, {}}
	tm := &tracedModel{LLM: &stubModel{name: "m", resps: resps}, name: "m"}

	n := 0
	for range tm.GenerateContent(context.Background(), &model.LLMRequest{}, true) {
		n++
		if n == 1 {
			break
		}
	}
	if n != 1 {
		t.Errorf("consumed %d responses, want the loop to stop after 1", n)
	}
}

func TestTracedModel_EmbedDelegatesWhenSupported(t *testing.T) {
	want := [][]float32{{1, 2, 3}}
	inner := &embeddableStub{stubModel: stubModel{name: "embed-model"}, vectors: want}
	tm := &tracedModel{LLM: inner, name: "embed-model"}

	got, err := tm.Embed(context.Background(), []string{"hello"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != 1 || len(got[0]) != 3 {
		t.Errorf("Embed() = %v, want %v", got, want)
	}
}

func TestTracedModel_EmbedErrorsWhenUnsupported(t *testing.T) {
	tm := &tracedModel{LLM: &stubModel{name: "no-embed"}, name: "no-embed"}
	if _, err := tm.Embed(context.Background(), []string{"hello"}); err == nil {
		t.Error("expected an error for a model that doesn't implement Embedder")
	}
}

// Ensure tracedModel satisfies Embedder (NewEmbedder's type assertion relies on this).
var _ Embedder = (*tracedModel)(nil)
