package orchestrator

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/vetting"
)

// ledgerCaptureExporter records every emitted log record - a local duplicate
// of dag_test's identically-named helper (unexported there; see that
// package's ask_advisor_test.go comment on why these can't be shared).
type ledgerCaptureExporter struct{ records []sdklog.Record }

func (c *ledgerCaptureExporter) Export(_ context.Context, records []sdklog.Record) error {
	c.records = append(c.records, records...)
	return nil
}
func (c *ledgerCaptureExporter) Shutdown(context.Context) error   { return nil }
func (c *ledgerCaptureExporter) ForceFlush(context.Context) error { return nil }

func ledgerAttrsOf(r sdklog.Record) map[string]string {
	out := map[string]string{}
	r.WalkAttributes(func(kv otellog.KeyValue) bool {
		if kv.Value.Kind() == otellog.KindString {
			out[string(kv.Key)] = kv.Value.AsString()
		}
		return true
	})
	return out
}

// newTracedTestOrch is newTestOrch (continue_test.go) with the orchestrator's
// OWN model wrapped and stamped the way production's inference.NewModel/serve.go do.
func newTracedTestOrch(t *testing.T, stub *orchStub) *Orchestrator {
	t.Helper()
	tracedModel := inference.TracedModelForTesting(stub, "orch-test-model")
	if da, ok := tracedModel.(interface{ SetDefaultAgent(string) }); ok {
		da.SetDefaultAgent(AgentName)
	}
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stub, Description: "researcher", Instruction: "ROLE:researcher",
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions,
		map[string]adkagent.Agent{"web-researcher": worker},
		map[string]model.LLM{"web-researcher": stub},
		vetting.NewJudgeFactory(stub, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher", Description: "researches the web"}}, nil, nil)
	return New(sessions, tracedModel, "You are the orchestrator.", planner, ex, nil, nil, nil)
}

// TestOrchestratorRun_RootChatCarriesChatID pins #617: before the fix, the
// orchestrator's own conversational "chat" ledger event carried NO
// gen_ai.conversation.id at all, so ledger.Exporter filed it into the shared
// "unscoped" bucket instead of this chat's stream - and with Node/Agent/Round
// also empty, it stays part of the root stream (StreamKey{}) replay.Session
// and eval key off.
func TestOrchestratorRun_RootChatCarriesChatID(t *testing.T) {
	capExp := &ledgerCaptureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	stub := &orchStub{replies: []*model.LLMResponse{stubText("Ducks are birds.")}}
	o := newTracedTestOrch(t, stub)

	const chatID = "root-stream-chat"
	for _, err := range o.Run(context.Background(), "u", chatID, SourceApp, "are ducks birds?", nil) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	var chatAttrs map[string]string
	for _, r := range capExp.records {
		attrs := ledgerAttrsOf(r)
		if attrs["gen_ai.operation.name"] == "chat" {
			chatAttrs = attrs
			break
		}
	}
	if chatAttrs == nil {
		t.Fatal("no chat ledger event recorded")
	}
	if got := chatAttrs["gen_ai.conversation.id"]; got != chatID {
		t.Errorf("root chat gen_ai.conversation.id = %q, want %q (would file into \"unscoped\" without the fix)", got, chatID)
	}
	if got := chatAttrs["quack.node"]; got != "" {
		t.Errorf("root chat quack.node = %q, want empty - the root stream must stay distinguishable (StreamKey{})", got)
	}
	if got := chatAttrs["gen_ai.agent.name"]; got != "" {
		t.Errorf("root chat gen_ai.agent.name = %q, want empty", got)
	}
}

// TestOrchestratorRun_SourceAndUserReachTokenUsageMetric drives Run's
// source/userID parameters end to end through the SAME production seam
// #617 above pins for gen_ai.conversation.id: ledger.WithCoords at Run's top
// -> tracedModel.GenerateContent -> recordUsageMetrics -> gen_ai.client.token.usage.
func TestOrchestratorRun_SourceAndUserReachTokenUsageMetric(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if err := otelobs.InitMetricsForTesting(mp.Meter("test")); err != nil {
		t.Fatalf("InitMetricsForTesting: %v", err)
	}

	reply := &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "Ducks are birds."}}},
		TurnComplete: true,
		FinishReason: genai.FinishReasonStop,
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     10,
			CandidatesTokenCount: 5,
		},
	}
	stub := &orchStub{replies: []*model.LLMResponse{reply}}
	o := newTracedTestOrch(t, stub)

	for _, err := range o.Run(context.Background(), "the-user", "source-chat", "github", "are ducks birds?", nil) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, met := range sm.Metrics {
			if met.Name != "gen_ai.client.token.usage" {
				continue
			}
			sum, ok := met.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("gen_ai.client.token.usage is not an int64 Sum")
			}
			for _, dp := range sum.DataPoints {
				userVal, _ := dp.Attributes.Value(attribute.Key("user"))
				sourceVal, _ := dp.Attributes.Value(attribute.Key("source"))
				if userVal.AsString() == "the-user" && sourceVal.AsString() == "github" {
					found = true
					if agentVal, ok := dp.Attributes.Value(attribute.Key("agent")); !ok || agentVal.AsString() != orchestratorName {
						t.Errorf("gen_ai.client.token.usage agent = %q, want %q - the orchestrator's own top-level model calls carry no agent attribution", agentVal.AsString(), orchestratorName)
					}
				}
			}
		}
	}
	if !found {
		t.Error("no gen_ai.client.token.usage data point carries user=the-user source=github - Run's source/userID params never reached tracedModel")
	}
}
