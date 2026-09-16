package dag_test

import (
	"context"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
)

// TestRunPlanAsGraph_JudgePromptProvenance pins #1420's split: a judge round
// runs on system/judge, so its llm.call records THAT artifact's version, while
// bundle.hash stays the worker's - whose answer is under review. The worker's
// own rows keep the worker's prompt version.
func TestRunPlanAsGraph_JudgePromptProvenance(t *testing.T) {
	capExp := &ledgerCaptureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	shippedJudge, err := artifactsrc.Static("system/judge")
	if err != nil {
		t.Fatalf("shipped system/judge: %v", err)
	}
	const (
		wantBundle       = "workerbundlehash"
		wantWorkerPrompt = "workerpromptv1"
	)

	stub := ledgerCoordsStub{}
	// Separate instances: SetLedgerCoords mutates the wrapper, so one shared
	// model would leave the judge round reading the worker round's last stamp.
	workerModel := inference.TracedModelForTesting(stub, "lc-worker-model")
	judgeModel := inference.TracedModelForTesting(stub, "lc-judge-model")
	builtins, err := tools.Build([]string{"current_date"}, tools.Deps{})
	if err != nil {
		t.Fatalf("tools.Build: %v", err)
	}
	worker, err := llmagent.New(llmagent.Config{
		Name: "w", Model: workerModel, Description: "w",
		Instruction: "ROLE:w Answer, calling current_date first.", Tools: builtins,
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}

	ex := dag.NewExecutor(session.InMemoryService(),
		map[string]adkagent.Agent{"w": lcScopedAgent{Agent: worker, model: workerModel, tools: builtins}},
		map[string]model.LLM{"w": workerModel},
		vetting.NewJudgeFactory(judgeModel, nil, nil),
		func(context.Context, string) vetting.Config {
			return vetting.Config{
				Threshold: 0.6, JudgeRounds: 1, JudgeModel: judgeModel,
				BundleHash: wantBundle, PromptSource: artifactsrc.StaticSource, PromptVersionID: wantWorkerPrompt,
				PromptArtifact: "system/code-reviewer",
			}
		}, nil)

	const chatID = "judge-provenance-chat"
	plan := dag.Plan{ID: "t", UserMessage: "what's today's date?", Nodes: []dag.Node{{ID: "n1", AgentName: "w", Task: "answer"}}}
	ctx := stream.WithYield(
		ledger.WithCoords(context.Background(), ledger.Coords{ChatID: chatID, User: "u", Source: "ui"}),
		func(stream.SSEEvent) {})
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: plan.UserMessage}}}
	if _, err := ex.RunPlanAsGraph(ctx, plan, "quack", "u", chatID, content, func(stream.SSEEvent, error) bool { return true }, map[string]string{}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	var judgeAttrs, workerAttrs map[string]string
	for _, r := range capExp.records {
		attrs := ledgerAttrsOf(r)
		if attrs["gen_ai.operation.name"] != "chat" {
			continue
		}
		switch attrs["gen_ai.agent.name"] {
		case "judge":
			if judgeAttrs == nil {
				judgeAttrs = attrs
			}
		case "w":
			if workerAttrs == nil {
				workerAttrs = attrs
			}
		}
	}

	if judgeAttrs == nil {
		t.Fatal("no judge llm.call recorded")
	}
	if got := judgeAttrs["quack.prompt.version_id"]; got != shippedJudge.VersionID {
		t.Errorf("judge quack.prompt.version_id = %q, want system/judge's %q (not the worker's)", got, shippedJudge.VersionID)
	}
	if got := judgeAttrs["quack.prompt.source"]; got != artifactsrc.StaticSource {
		t.Errorf("judge quack.prompt.source = %q, want %q", got, artifactsrc.StaticSource)
	}
	if got := judgeAttrs["quack.bundle.hash"]; got != wantBundle {
		t.Errorf("judge quack.bundle.hash = %q, want the worker's %q - whose answer is under review", got, wantBundle)
	}
	// #1422 N3: a typo in the attribute key must not silently disable pinning.
	if got := judgeAttrs["quack.prompt.artifact"]; got != "system/judge" {
		t.Errorf("judge quack.prompt.artifact = %q, want %q", got, "system/judge")
	}

	if workerAttrs == nil {
		t.Fatal("no worker llm.call recorded")
	}
	if got := workerAttrs["quack.prompt.version_id"]; got != wantWorkerPrompt {
		t.Errorf("worker quack.prompt.version_id = %q, want %q", got, wantWorkerPrompt)
	}
	if got := workerAttrs["quack.bundle.hash"]; got != wantBundle {
		t.Errorf("worker quack.bundle.hash = %q, want %q", got, wantBundle)
	}
	if got := workerAttrs["quack.prompt.artifact"]; got != "system/code-reviewer" {
		t.Errorf("worker quack.prompt.artifact = %q, want %q", got, "system/code-reviewer")
	}
}
