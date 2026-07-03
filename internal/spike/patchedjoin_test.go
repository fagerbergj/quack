package spike

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync/atomic"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

// THE WORKAROUND SPIKE: ADK's buildRunState only rehydrates NodeStates for nodes
// that raised interrupts, so a completed non-asker sibling has state.Nodes[name] ==
// nil and a downstream JoinNode can never satisfy its barrier on resume
// (aggregatePredecessorOutputs: nil → not ready). This spike wraps the workflow in
// a custom agent (agent.New) that replicates workflowagent's detectResume and
// PATCHES completed siblings into state.Nodes (EnsureNode + NodeCompleted + Output
// from session history) before calling Resume. If the join then settles in-stream,
// the native DAG + HITL + JoinNode + synthesizer-as-node shape is fully viable.
func TestNativeGraph_PatchedJoinResume(t *testing.T) {
	const interruptID = "askP2"
	var bRuns, synthRuns atomic.Int32

	askA := workflow.NewEmittingFunctionNode[any, any]("askA",
		func(nc adkagent.Context, _ any, emit func(*session.Event) error) (any, error) {
			if err := emit(workflow.NewRequestInputEvent(nc, session.RequestInput{
				InterruptID: interruptID, Message: "which direction?",
			})); err != nil {
				return nil, err
			}
			return nil, workflow.ErrNodeInterrupted
		}, workflow.NodeConfig{}) // handoff: the reply becomes askA's output
	workerB := workflow.NewFunctionNode[any, any]("workerB",
		func(adkagent.Context, any) (any, error) { bRuns.Add(1); return "B", nil }, workflow.NodeConfig{})
	join := workflow.NewJoinNode("join")
	synth := workflow.NewFunctionNode[any, any]("synth",
		func(_ adkagent.Context, in any) (any, error) {
			synthRuns.Add(1)
			m, _ := in.(map[string]any)
			a, _ := m["askA"].(string)
			b, _ := m["workerB"].(string)
			return "synth(" + a + "|" + b + ")", nil
		}, workflow.NodeConfig{})

	edges := workflow.NewEdgeBuilder().
		AddFanOut(workflow.Start, askA, workerB).
		AddFanIn(join, askA, workerB).
		Add(join, synth).Build()
	wf, err := workflow.New("pj", edges)
	if err != nil {
		t.Fatalf("workflow: %v", err)
	}
	nodeNames := map[string]bool{"askA": true, "workerB": true, "join": true, "synth": true}

	wrapper, err := adkagent.New(adkagent.Config{
		Name: "pj", Description: "patched workflow wrapper",
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				// detectResume replica: FunctionResponses named adk_request_input.
				responses := map[string]any{}
				if uc := ctx.UserContent(); uc != nil {
					for _, p := range uc.Parts {
						if p == nil || p.FunctionResponse == nil || p.FunctionResponse.Name != workflow.WorkflowInputFunctionCallName {
							continue
						}
						resp := p.FunctionResponse.Response
						if v, ok := resp["payload"]; ok {
							responses[p.FunctionResponse.ID] = v
						} else if v, ok := resp["response"]; ok {
							responses[p.FunctionResponse.ID] = v
						} else {
							responses[p.FunctionResponse.ID] = resp
						}
					}
				}
				if len(responses) > 0 {
					state, err := wf.ReconstructRunState(ctx.Session(), ctx.InvocationID())
					if err != nil {
						yield(nil, err)
						return
					}
					if state != nil {
						// THE PATCH: completed non-asker siblings → NodeCompleted+Output,
						// read from this invocation's session history.
						patchCompletedSiblings(state, ctx.Session(), ctx.InvocationID(), nodeNames)
						for ev, err := range wf.Resume(adkagent.Promote(ctx), state, responses) {
							if !yield(ev, err) {
								return
							}
						}
						return
					}
				}
				for ev, err := range wf.Run(ctx) {
					if !yield(ev, err) {
						return
					}
				}
			}
		},
	})
	if err != nil {
		t.Fatalf("wrapper: %v", err)
	}

	sessions := session.InMemoryService()
	newRunner := func() *runner.Runner {
		r, _ := runner.New(runner.Config{AppName: "pj", Agent: wrapper, SessionService: sessions, AutoCreateSession: true})
		return r
	}
	ctx := context.Background()

	var paused bool
	for ev, err := range newRunner().Run(ctx, "u", "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
		if err != nil && !errors.Is(err, workflow.ErrNodeInterrupted) {
			t.Fatalf("run1: %v", err)
		}
		if ev != nil && ev.RequestedInput != nil {
			paused = true
		}
	}
	if !paused {
		t.Fatal("run1: no pause")
	}

	answer := &genai.Content{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
		ID: interruptID, Name: workflow.WorkflowInputFunctionCallName,
		Response: map[string]any{"payload": "north"},
	}}}}
	var finalText string
	for ev, err := range newRunner().Run(ctx, "u", "s", answer, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run2: %v", err)
		}
		if ev == nil {
			continue
		}
		p := ""
		if ev.NodeInfo != nil {
			p = ev.NodeInfo.Path
		}
		if s, ok := ev.Output.(string); ok && s != "" {
			finalText = s
		}
		t.Logf("run2 ev: path=%q output=%v", p, ev.Output)
	}
	t.Logf("RESULT patched-join: bRuns=%d synthRuns=%d final=%q", bRuns.Load(), synthRuns.Load(), finalText)
	if bRuns.Load() != 1 {
		t.Errorf("workerB re-ran: %d", bRuns.Load())
	}
	if synthRuns.Load() != 1 || !strings.Contains(finalText, "synth(north|B)") {
		t.Errorf("patched join did not settle: synthRuns=%d final=%q", synthRuns.Load(), finalText)
	}
}

// patchCompletedSiblings backfills state.Nodes entries for graph nodes that
// completed with an output in THIS invocation's history but got no rehydrated
// NodeState (ADK's buildRunState only covers interrupt-raising nodes). Without
// this a JoinNode downstream of a paused node can never fire on resume.
func patchCompletedSiblings(state *workflow.RunState, sess session.Session, invocationID string, nodeNames map[string]bool) {
	if state == nil || sess == nil {
		return
	}
	for ev := range sess.Events().All() {
		if ev == nil || ev.Output == nil || ev.NodeInfo == nil || ev.InvocationID != invocationID {
			continue
		}
		name := staticNodeNameFromPath(ev.NodeInfo.Path, nodeNames)
		if name == "" || state.Nodes[name] != nil {
			continue
		}
		ns := state.EnsureNode(name)
		ns.Status = workflow.NodeCompleted
		ns.Output = ev.Output
		ns.Branch = ev.Branch
	}
}

// staticNodeNameFromPath finds the graph-node name in a NodeInfo path like
// "pj@1/workerB@1" (segments are name@run; dynamic children fold into their
// static ancestor — first known segment wins).
func staticNodeNameFromPath(path string, known map[string]bool) string {
	for _, seg := range strings.Split(path, "/") {
		if i := strings.IndexByte(seg, '@'); i >= 0 {
			seg = seg[:i]
		}
		if known[seg] {
			return seg
		}
	}
	return ""
}

// Same patch, but the asker is a RE-ENTRY dynamic node (RerunOnResume) that calls
// ResumeOrRequestInput inside its body and produces a REAL output from the reply —
// the exact shape of quack's gated worker nodes (the gate re-runs the worker with
// the user's answer). Proves the patched join settles for re-entry askers too.
func TestNativeGraph_PatchedJoinResume_Reentry(t *testing.T) {
	const interruptID = "askP3"
	var bRuns, synthRuns, aResumes atomic.Int32
	rerun := true

	workerA := workflow.NewEmittingFunctionNode[any, any]("workerA",
		func(nc adkagent.Context, _ any, emit func(*session.Event) error) (any, error) {
			reply, err := workflow.ResumeOrRequestInput(nc, emit, session.RequestInput{
				InterruptID: interruptID, Message: "which direction?",
			})
			if err != nil {
				return nil, err
			}
			aResumes.Add(1)
			s, _ := reply.(string)
			return "A-researched-" + s, nil // the node CONTINUES with the answer
		}, workflow.NodeConfig{RerunOnResume: &rerun})
	workerB := workflow.NewFunctionNode[any, any]("workerB",
		func(adkagent.Context, any) (any, error) { bRuns.Add(1); return "B", nil }, workflow.NodeConfig{})
	join := workflow.NewJoinNode("join")
	synth := workflow.NewFunctionNode[any, any]("synth",
		func(_ adkagent.Context, in any) (any, error) {
			synthRuns.Add(1)
			m, _ := in.(map[string]any)
			a, _ := m["workerA"].(string)
			b, _ := m["workerB"].(string)
			return "synth(" + a + "|" + b + ")", nil
		}, workflow.NodeConfig{})

	edges := workflow.NewEdgeBuilder().
		AddFanOut(workflow.Start, workerA, workerB).
		AddFanIn(join, workerA, workerB).
		Add(join, synth).Build()
	wf, err := workflow.New("pj3", edges)
	if err != nil {
		t.Fatalf("workflow: %v", err)
	}
	nodeNames := map[string]bool{"workerA": true, "workerB": true, "join": true, "synth": true}
	wrapper := newPatchedWrapper(t, "pj3", wf, nodeNames)

	sessions := session.InMemoryService()
	newRunner := func() *runner.Runner {
		r, _ := runner.New(runner.Config{AppName: "pj3", Agent: wrapper, SessionService: sessions, AutoCreateSession: true})
		return r
	}
	ctx := context.Background()
	for ev, err := range newRunner().Run(ctx, "u", "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
		if err != nil && !errors.Is(err, workflow.ErrNodeInterrupted) {
			t.Fatalf("run1: %v", err)
		}
		_ = ev
	}
	answer := &genai.Content{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
		ID: interruptID, Name: workflow.WorkflowInputFunctionCallName,
		Response: map[string]any{"payload": "north"},
	}}}}
	var finalText string
	for ev, err := range newRunner().Run(ctx, "u", "s", answer, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run2: %v", err)
		}
		if ev == nil {
			continue
		}
		if s, ok := ev.Output.(string); ok && s != "" {
			finalText = s
		}
	}
	t.Logf("RESULT patched-join-reentry: bRuns=%d aResumes=%d synthRuns=%d final=%q", bRuns.Load(), aResumes.Load(), synthRuns.Load(), finalText)
	if bRuns.Load() != 1 || aResumes.Load() != 1 || synthRuns.Load() != 1 || !strings.Contains(finalText, "synth(A-researched-north|B)") {
		t.Errorf("re-entry patched join failed: bRuns=%d aResumes=%d synthRuns=%d final=%q",
			bRuns.Load(), aResumes.Load(), synthRuns.Load(), finalText)
	}
}

// newPatchedWrapper builds the thin custom agent that replicates workflowagent's
// resume dispatch and applies patchCompletedSiblings before Resume.
func newPatchedWrapper(t *testing.T, name string, wf *workflow.Workflow, nodeNames map[string]bool) adkagent.Agent {
	t.Helper()
	wrapper, err := adkagent.New(adkagent.Config{
		Name: name, Description: "patched workflow wrapper",
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				responses := map[string]any{}
				if uc := ctx.UserContent(); uc != nil {
					for _, p := range uc.Parts {
						if p == nil || p.FunctionResponse == nil || p.FunctionResponse.Name != workflow.WorkflowInputFunctionCallName {
							continue
						}
						resp := p.FunctionResponse.Response
						if v, ok := resp["payload"]; ok {
							responses[p.FunctionResponse.ID] = v
						} else if v, ok := resp["response"]; ok {
							responses[p.FunctionResponse.ID] = v
						} else {
							responses[p.FunctionResponse.ID] = resp
						}
					}
				}
				if len(responses) > 0 {
					state, err := wf.ReconstructRunState(ctx.Session(), ctx.InvocationID())
					if err != nil {
						yield(nil, err)
						return
					}
					if state != nil {
						patchCompletedSiblings(state, ctx.Session(), ctx.InvocationID(), nodeNames)
						for ev, err := range wf.Resume(adkagent.Promote(ctx), state, responses) {
							if !yield(ev, err) {
								return
							}
						}
						return
					}
				}
				for ev, err := range wf.Run(ctx) {
					if !yield(ev, err) {
						return
					}
				}
			}
		},
	})
	if err != nil {
		t.Fatalf("wrapper: %v", err)
	}
	return wrapper
}
