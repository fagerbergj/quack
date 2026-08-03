// Package replaytest is the test harness for internal/replay: given a
// recorded bundle, it drives ONE gated node (vetting.RunGatedRefine)
// through replay-backed models and tool stubs, and returns the outcome
// alongside the session's divergence report.
//
// Scope ceiling: this harness drives a single vetting.RunGatedRefine node,
// not the full DAG (internal/dag) - wiring replay into the full orchestrator
// is left as a follow-up. The replay ENGINE itself (replay.Session, the
// kind:replay provider, the tool stubs, the divergence report) has no
// dependency on which harness drives it.
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

// Options configures the single gated node Run drives. It mirrors what the
// recording's own RunGatedRefine call used: replay only ever replays a call
// the recording actually made, so an Options that names a different NodeID,
// model, or tool list than was recorded reads back as ordinary structural
// divergence (an "extra call" MissError), not a harness bug.
type Options struct {
	// NodeID is both RunGatedRefine's nodeID argument and Cfg.NodeID -
	// they must agree (the worker round keys its ledger coordinates off
	// Cfg.NodeID, the judge round off the nodeID argument - see
	// internal/vetting/node.go).
	NodeID string
	// WorkerModel/JudgeModel are the model names bound at record time
	// (gen_ai.request.model) - what NextChat matches identity against.
	WorkerModel string
	JudgeModel  string
	// ToolNames are the worker agent's tool list, by name - each becomes a
	// replay stub (tools.Deps.Replayer) answering from the recording.
	ToolNames []string
	// Instruction is the worker agent's system instruction - part of the
	// prompt-version hash replay.Session.NextChat compares against the
	// recording (Session.NextChat's drift check). Match the recording's
	// instruction verbatim for a "replays green, zero drift" case; change
	// it deliberately to exercise the "prompt edit tolerated" case.
	Instruction string
	// Cfg is the gate config the recording ran under (JudgeRounds,
	// Threshold, etc.) - Cfg.ChatID/NodeID/Agent are required.
	Cfg vetting.Config
	// Prompt overrides the fed-in user turn. "" derives it from the
	// bundle's recorded root user message (replay.Session.UserTurn).
	Prompt string
}

// Outcome is what the driven node produced, plus the session's divergence
// report - everything a replaytest assertion needs.
type Outcome struct {
	Answer string
	Result vetting.GateResult
	// Err is RunGatedRefine's OWN returned error, captured directly from its
	// return value rather than inferred from the ADK runner's event
	// iteration - a raw model/tool-call error inside a sub-agent can be
	// swallowed into a silent empty completion by ADK's own scheduler, so
	// this is the one place that error is guaranteed visible.
	Err error
	// Report is the replay session's full divergence accounting at the end
	// of the run - Report.Clean() is what "replays green" means.
	Report replay.Report
}

// Run loads bundlePath, builds the worker+judge as replay-backed models
// (inference.NewReplayModel) and the worker's tools as replay stubs
// (tools.Build with Deps.Replayer set), and drives ONE
// vetting.RunGatedRefine node against them with opts.
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
		// The gate's own error is captured above, not returned to the
		// runner: ADK's own error handling for a failed sub-agent run is
		// not this package's concern, and returning it here risks the
		// runner masking it into a silent empty completion instead.
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
		_ = rerr // best-effort only; out.Err (captured directly above) is authoritative
	}

	out.Report = sess.Report()
	return out
}
