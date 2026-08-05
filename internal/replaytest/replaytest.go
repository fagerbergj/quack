// Package replaytest drives one gated node through replay-backed models and tool stubs.
package replaytest

import (
	"context"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/replay"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
)

// Options mirrors what the recording's Own RunGatedRefine call used.
type Options struct {
	NodeID      string
	WorkerModel string
	JudgeModel  string
	ToolNames   []string
	Instruction string
	Cfg         vetting.Config
	Prompt      string
}

// Outcome is the node's result plus the session's divergence report.
type Outcome struct {
	Answer string
	Result vetting.GateResult
	Err    error
	Report replay.Report
}

// Run loads bundlePath, builds replay-backed models and tool stubs, and drives
// one vetting.RunGatedRefine node against them.
func Run(t testing.TB, bundlePath string, opts Options) Outcome {
	t.Helper()
	sess, err := replay.Load(bundlePath)
	if err != nil {
		t.Fatalf("replaytest: load bundle %q: %v", bundlePath, err)
	}

	prompt := opts.Prompt
	if prompt == "" {
		if p, ok := sess.UserTurn(); ok {
			prompt = p
		}
	}

	workerModel := inference.NewReplayModel(sess, opts.WorkerModel)
	judgeModel := inference.NewReplayModel(sess, opts.JudgeModel)

	toolCoords := ledger.Coords{ChatID: opts.Cfg.ChatID, Node: opts.NodeID, Agent: opts.Cfg.Agent}
	toolList, err := tools.Build(opts.ToolNames, tools.Deps{Replayer: sess, LedgerCoords: toolCoords})
	if err != nil {
		t.Fatalf("replaytest: build tool stubs: %v", err)
	}

	worker, err := llmagent.New(llmagent.Config{
		Name: opts.Cfg.Agent, Model: workerModel, Description: "replayed worker",
		Instruction: opts.Instruction, Tools: toolList,
	})
	if err != nil {
		t.Fatalf("replaytest: build worker agent: %v", err)
	}
	workerNode, err := vetting.NewWorkerNode(worker)
	if err != nil {
		t.Fatalf("replaytest: build worker node: %v", err)
	}
	judgeFactory := vetting.NewJudgeFactory(judgeModel, nil, nil)

	var out Outcome
	fn := func(ctx adkagent.Context, task string, emit func(*session.Event) error) (string, error) {
		if task == "" {
			task = prompt
		}
		out.Answer, out.Result, out.Err = vetting.RunGatedRefine(ctx, opts.NodeID, workerNode, workerModel, judgeFactory, opts.Cfg, task, nil, nil, emit)
		return out.Answer, nil
	}
	node := workflow.NewDynamicNode[string, string](opts.NodeID, fn, workflow.NodeConfig{})

	root, err := workflowagent.New(workflowagent.Config{
		Name: "replaytest-root", SubAgents: []adkagent.Agent{worker},
		Edges: workflow.Chain(workflow.Start, node),
	})
	if err != nil {
		t.Fatalf("replaytest: build root workflow: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName: "replaytest", Agent: root,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("replaytest: build runner: %v", err)
	}

	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: prompt}}}
	for _, rerr := range r.Run(context.Background(), "u", opts.Cfg.ChatID, task, adkagent.RunConfig{}) {
		_ = rerr
	}

	out.Report = sess.Report()
	return out
}
