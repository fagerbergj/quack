package orchestrator

import (
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
)

// fakeModel is a canned model.LLM that emits one reasoning part + one answer
// part, so we can exercise the orchestrator → ADK runner → Translate pipe with
// no network and no database.
type fakeModel struct{}

func (fakeModel) Name() string { return "fake" }

func (fakeModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content: &genai.Content{Role: "model", Parts: []*genai.Part{
				{Text: "let me think", Thought: true},
				{Text: "Hello!"},
			}},
			TurnComplete: true,
		}, nil)
	}
}

func TestOrchestratorRunStreamsLabeledEvents(t *testing.T) {
	o, err := New(fakeModel{}, session.InMemoryService(), "be helpful", nil)
	if err != nil {
		t.Fatal(err)
	}

	var thinking, answer string
	for ev, err := range o.Run(context.Background(), "local", "s1", "hi") {
		if err != nil {
			t.Fatal(err)
		}
		for _, se := range stream.Translate(ev) {
			switch d := se.Data.(type) {
			case stream.ThinkingData:
				thinking += d.Text
			case stream.TokenData:
				answer += d.Text
			}
		}
	}

	if thinking != "let me think" {
		t.Errorf("thinking = %q, want %q", thinking, "let me think")
	}
	if !strings.Contains(answer, "Hello!") {
		t.Errorf("answer = %q, want it to contain %q", answer, "Hello!")
	}
}
