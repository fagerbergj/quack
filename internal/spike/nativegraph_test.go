// Package spike is a throwaway, deterministic proof (no LLM, no network) that ADK
// v2's native first-class-node graph can replace quack's custom runDAG for the
// node-HITL work. It answers the ONE question the ADK2 migration spike left open
// (.quack/adk2-spike-findings.md line 143): when several first-class workers fan
// out and one PAUSES for human input, does resume skip the already-finished
// siblings INDEPENDENTLY (durable), re-enter only the paused one, and settle a
// JoinNode fan-in?
//
// Shape:  Start ──▶ workerA (pauses: ResumeOrRequestInput)
//              └──▶ workerB (completes immediately, counts its runs)
//         workerA, workerB ──▶ join ──▶ synth (counts its runs)
//
// Run 1 parks at workerA. Run 2 (fresh runner, same session store) feeds the
// answer. We assert workerB did NOT re-run and synth ran exactly once.
package spike

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

func TestNativeGraph_FanOutHITLResume_SkipsFinishedSiblings(t *testing.T) {
	const interruptID = "askA" // correlation key for workerA's pause/resume

	var bRuns, synthRuns, aResumes atomic.Int32
	rerun := true

	// workerA: first activation pauses for input; on resume it returns the reply.
	workerA := workflow.NewEmittingFunctionNode[any, any]("workerA",
		func(nc adkagent.Context, _ any, emit func(*session.Event) error) (any, error) {
			reply, err := workflow.ResumeOrRequestInput(nc, emit, session.RequestInput{
				InterruptID: interruptID,
				Message:     "which direction?",
			})
			if err != nil {
				return nil, err // ErrNodeInterrupted on the first pass → park
			}
			aResumes.Add(1)
			return "A:" + toStr(reply), nil
		}, workflow.NodeConfig{RerunOnResume: &rerun})

	// workerB: independent sibling that finishes on the first pass. If resume
	// re-runs it, bRuns climbs past 1 — that's the regression we're guarding.
	workerB := workflow.NewFunctionNode[any, any]("workerB",
		func(_ adkagent.Context, _ any) (any, error) {
			bRuns.Add(1)
			return "B", nil
		}, workflow.NodeConfig{})

	join := workflow.NewJoinNode("join")

	synth := workflow.NewFunctionNode[any, any]("synth",
		func(_ adkagent.Context, in any) (any, error) {
			synthRuns.Add(1)
			return "synth(" + toStr(in) + ")", nil
		}, workflow.NodeConfig{})

	edges := workflow.NewEdgeBuilder().
		AddFanOut(workflow.Start, workerA, workerB).
		AddFanIn(join, workerA, workerB).
		Add(join, synth).
		Build()

	wf, err := workflowagent.New(workflowagent.Config{Name: "spike", Edges: edges})
	if err != nil {
		t.Fatalf("workflow: %v", err)
	}

	// One session store shared across both runners = "fresh runner, same store".
	sessions := session.InMemoryService()
	const userID, sessID = "u", "s"
	newRunner := func() *runner.Runner {
		r, err := runner.New(runner.Config{
			AppName: "spike", Agent: wf, SessionService: sessions, AutoCreateSession: true,
		})
		if err != nil {
			t.Fatalf("runner: %v", err)
		}
		return r
	}

	// ---- Run 1: fresh start, should park at workerA ----
	ctx := context.Background()
	start := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}
	var sawPause bool
	for ev, err := range newRunner().Run(ctx, userID, sessID, start, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run1: %v", err)
		}
		if ev != nil && ev.RequestedInput != nil {
			sawPause = true
			if ev.RequestedInput.InterruptID != interruptID {
				t.Fatalf("run1: pause interruptID = %q, want %q", ev.RequestedInput.InterruptID, interruptID)
			}
		}
	}
	if !sawPause {
		t.Fatal("run1: never saw a RequestedInput pause event")
	}
	if got := bRuns.Load(); got != 1 {
		t.Fatalf("run1: workerB ran %d times, want 1", got)
	}
	if got := synthRuns.Load(); got != 0 {
		t.Fatalf("run1: synth ran %d times before the join settled, want 0", got)
	}

	// ---- Run 2: resume by delivering workerA's answer as a FunctionResponse ----
	answer := &genai.Content{Role: "user", Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{
			ID:       interruptID,
			Name:     workflow.WorkflowInputFunctionCallName,
			Response: map[string]any{"payload": "north"},
		},
	}}}
	var finalText string
	for ev, err := range newRunner().Run(ctx, userID, sessID, answer, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run2: %v", err)
		}
		if ev == nil {
			continue
		}
		path := ""
		if ev.NodeInfo != nil {
			path = ev.NodeInfo.Path
		}
		txt, out := "", ""
		if ev.Content != nil {
			for _, p := range ev.Content.Parts {
				if p.Text != "" {
					txt = p.Text
					finalText = p.Text
				}
			}
		}
		if ev.Output != nil {
			out = fmt.Sprintf("%v", ev.Output)
			if s, ok := ev.Output.(string); ok && s != "" {
				finalText = s
			}
		}
		t.Logf("run2 ev: author=%s path=%q text=%q output=%q reqInput=%v final=%v",
			ev.Author, path, txt, out, ev.RequestedInput != nil, ev.IsFinalResponse())
	}

	// Proven-good: finished sibling durably skipped, paused worker resumed once.
	if got := bRuns.Load(); got != 1 {
		t.Errorf("workerB re-ran on resume (ran %d total) — finished siblings are NOT durably skipped", got)
	}
	if got := aResumes.Load(); got != 1 {
		t.Errorf("workerA resumed %d times, want exactly 1", got)
	}
	// DOCUMENTED DEFECT (ADK v2.0.0): the JoinNode does NOT settle in-stream on HITL
	// resume when a predecessor was skipped — run2 yields only the resumed worker,
	// so the terminal synth's answer is NOT delivered in the resume stream. CANARY:
	// if a future ADK bump fixes JoinNode resume, finalText gains "synth(" and this
	// flips red — revisit .quack/node-hitl-spike.md (Path A′ could use JoinNodes).
	if strings.Contains(finalText, "synth(") {
		t.Errorf("JoinNode resume appears FIXED (final=%q) — revisit the no-join workaround", finalText)
	}
	t.Logf("documented: bRuns=1 (skipped), aResumes=1, join did NOT drive synth in-stream, final=%q", finalText)
}

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
