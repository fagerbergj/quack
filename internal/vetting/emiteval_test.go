package vetting

import (
	"context"
	"iter"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

type captureEvalExporter struct{ records []sdklog.Record }

func (c *captureEvalExporter) Export(_ context.Context, records []sdklog.Record) error {
	c.records = append(c.records, records...)
	return nil
}
func (c *captureEvalExporter) Shutdown(context.Context) error   { return nil }
func (c *captureEvalExporter) ForceFlush(context.Context) error { return nil }

// TestEmitEvaluationResults_DeterministicOrder guards against map iteration
// (v.Criteria) making the emitted event order flap run to run - a
// replay/diff consumer needs a stable order, not just a stable set.
func TestEmitEvaluationResults_DeterministicOrder(t *testing.T) {
	capExp := &captureEvalExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	v := verdict{Criteria: map[string]criterionScore{
		"zebra_criterion":  {Score: 0.5, Reason: "z"},
		"alpha_criterion":  {Score: 0.9, Reason: "a"},
		"middle_criterion": {Score: 0.7, Reason: "m"},
	}}

	for i := 0; i < 5; i++ {
		capExp.records = nil
		emitEvaluationResults(context.Background(), "judge-r1", v)
		if len(capExp.records) != 3 {
			t.Fatalf("iteration %d: got %d records, want 3", i, len(capExp.records))
		}
		var names []string
		for _, r := range capExp.records {
			r.WalkAttributes(func(kv attribute.KeyValue) bool {
				if string(kv.Key) == otelobs.GenAIEvaluationName {
					names = append(names, kv.Value.AsString())
				}
				return true
			})
		}
		want := []string{"alpha_criterion", "middle_criterion", "zebra_criterion"}
		if len(names) != len(want) {
			t.Fatalf("iteration %d: got %v, want %v", i, names, want)
		}
		for j := range want {
			if names[j] != want[j] {
				t.Errorf("iteration %d: order = %v, want alphabetical %v", i, names, want)
				break
			}
		}
	}
}

// TestEmitEvaluationResults_AgentAttribute: per-criterion events must carry
// gen_ai.agent.name (ledger's only "per agent" query key) when ctx carries
// it - previously only response id/name/score/explanation were stamped.
func TestEmitEvaluationResults_AgentAttribute(t *testing.T) {
	capExp := &captureEvalExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	v := verdict{Criteria: map[string]criterionScore{"accurate": {Score: 1.0, Reason: "ok"}}}
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{Agent: "reviewer"})
	emitEvaluationResults(ctx, "judge-r1", v)

	if len(capExp.records) != 1 {
		t.Fatalf("got %d records, want 1", len(capExp.records))
	}
	var got string
	capExp.records[0].WalkAttributes(func(kv attribute.KeyValue) bool {
		if string(kv.Key) == otelobs.GenAIAgentName {
			got = kv.Value.AsString()
		}
		return true
	})
	if got != "reviewer" {
		t.Errorf("gen_ai.agent.name = %q, want %q", got, "reviewer")
	}
}

// usageLLM answers every verifier call with a reply whose usage reports a cached prefix.
type usageLLM struct{}

func (usageLLM) Name() string { return "usage-llm" }

func (usageLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: `{"items":[]}`}}}, TurnComplete: true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 1000, CachedContentTokenCount: 816, CandidatesTokenCount: 20}}, nil)
	}
}

// TestVerifierCallRecordsCachedTokens: the verifier's calls go through the judge model's traced
// wrapper, so its llm.call entries carry the same cached/input split as every other call.
func TestVerifierCallRecordsCachedTokens(t *testing.T) {
	capExp := &captureEvalExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()
	Verifier{LLM: inference.TracedModelForTesting(usageLLM{}, "judge")}.VerifyChecks(context.Background(), []UnitCheck{secondLookCheck()})
	var cached, input int64
	for _, r := range capExp.records {
		r.WalkAttributes(func(kv attribute.KeyValue) bool {
			switch string(kv.Key) {
			case otelobs.GenAIUsageCachedTokens:
				cached = kv.Value.AsInt64()
			case otelobs.GenAIUsageInputTokens:
				input = kv.Value.AsInt64()
			}
			return true
		})
	}
	if cached != 816 || input != 184 {
		t.Errorf("verifier llm.call cached=%d input=%d, want 816 and 184", cached, input)
	}
}
