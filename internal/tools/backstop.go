package tools

import (
	"context"

	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// backstopMetaAnswer is delivered to a node the orchestrator left paused: the model
// neither answered the request_input nor escalated it to the user, so the system
// tells the worker no answer is coming and to finish best-effort.
const backstopMetaAnswer = "No answer was provided to your question(s) — the orchestrator did not respond. " +
	"Produce the best result you can with the information you have and clearly state what is missing. Do not ask again."

// maxBackstopResumes bounds the meta-answer loop if a node keeps re-asking, so the
// backstop always terminates.
const maxBackstopResumes = 2

// BackstopResume deterministically completes a DAG the model left paused. It resumes
// the pending node with backstopMetaAnswer (no real input), forwards node activity to
// yield, and loops — bounded by maxBackstopResumes — if the node re-pauses. Returns
// the terminal answer once the DAG finishes, or "" if it couldn't within the cap.
func BackstopResume(ctx context.Context, executor *dag.Executor, userID string, pending *PendingInput, yield func(stream.SSEEvent)) string {
	p := pending
	for i := 0; i < maxBackstopResumes && p != nil; i++ {
		content := &genai.Content{Role: "user", Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				ID:       p.CallID,
				Name:     RequestInputToolName,
				Response: map[string]any{RequestInputAnswerKey: []string{backstopMetaAnswer}},
			},
		}}}
		nodeOutputs := make(map[string]string)
		waiting, err := drainDAG(executor.Resume(ctx, p.Plan, userID, nodeOutputs, p.Done, p.Waiting, p.NodeID, content), yield)
		if err != nil {
			return ""
		}
		if len(waiting) == 0 {
			return TerminalOutput(p.Plan, nodeOutputs)
		}
		p = buildPending(p.Plan, nodeOutputs, waiting) // re-paused — try again, firmer cap
	}
	return ""
}

// HydratePending seeds the cache's pending input from the store if a prior turn left a
// node waiting (the in-memory snapshot is gone across turns). This lets the end-of-turn
// backstop catch a CROSS-turn drop — the model escalated, the user replied, but the
// model never resumed the node. No-op when nothing is waiting.
func HydratePending(ctx context.Context, cache *PlanCache, st *store.Store, appName, userID, sessionID string) {
	turns, err := st.GetTurnsWithContent(ctx, appName, userID, sessionID)
	if err != nil {
		return
	}
	for i := range turns {
		tc := &turns[i]
		if tc.Plan == nil {
			continue
		}
		for j := range tc.Nodes {
			if tc.Nodes[j].Status != "waiting" {
				continue
			}
			plan, done, waiting, callID, lerr := loadResumeState(ctx, st, appName, userID, sessionID, tc.Plan.ID, tc.Nodes[j].NodeID)
			if lerr != nil {
				return
			}
			cache.SetPending(&PendingInput{Plan: plan, Done: done, Waiting: waiting, NodeID: tc.Nodes[j].NodeID, CallID: callID})
			return
		}
	}
}
