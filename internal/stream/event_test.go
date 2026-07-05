package stream

import (
	"encoding/json"
	"testing"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func eventWith(parts ...*genai.Part) *session.Event {
	e := &session.Event{}
	e.Content = &genai.Content{Parts: parts}
	return e
}

func TestTranslatorRunLifecycle(t *testing.T) {
	tr := NewTranslator()

	// agent_start opens the run.
	got := tr.Event(eventWith(AgentStartPart("r1", "web-researcher", StageWorker, 0)))
	if len(got) != 1 || got[0].Name != EventAgentStart {
		t.Fatalf("start = %+v, want one agent_start", got)
	}
	if d, ok := got[0].Data.(AgentStartData); !ok || d.RunID != "r1" || d.Agent != "web-researcher" || d.Stage != StageWorker {
		t.Errorf("agent_start data = %+v", got[0].Data)
	}

	// Thinking is attributed to the open run.
	got = tr.Event(eventWith(&genai.Part{Text: "reasoning", Thought: true}))
	if len(got) != 1 || got[0].Name != EventAgentThinking {
		t.Fatalf("thinking = %+v", got)
	}
	if d, ok := got[0].Data.(AgentThinkingData); !ok || d.RunID != "r1" || d.Text != "reasoning" {
		t.Errorf("agent_thinking data = %+v", got[0].Data)
	}

	// Tool call + result pair by call ID, tagged with the run.
	got = tr.Event(eventWith(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "web_search", Args: map[string]any{}}}))
	if d, ok := got[0].Data.(AgentToolCallData); !ok || d.RunID != "r1" || d.CallID != "c1" || d.Name != "web_search" {
		t.Errorf("agent_tool_call data = %+v", got[0].Data)
	}
	got = tr.Event(eventWith(&genai.Part{FunctionResponse: &genai.FunctionResponse{ID: "c1", Name: "web_search", Response: map[string]any{"result": "x"}}}))
	if d, ok := got[0].Data.(AgentToolResultData); !ok || d.RunID != "r1" || d.CallID != "c1" {
		t.Errorf("agent_tool_result data = %+v", got[0].Data)
	}

	// agent_complete closes the run.
	got = tr.Event(eventWith(AgentCompletePart(AgentCompleteData{RunID: "r1", Stage: StageWorker})))
	if len(got) != 1 || got[0].Name != EventAgentComplete {
		t.Fatalf("complete = %+v", got)
	}
	if d, ok := got[0].Data.(AgentCompleteData); !ok || d.RunID != "r1" || d.Stage != StageWorker {
		t.Errorf("agent_complete data = %+v", got[0].Data)
	}

	// After the run closes, plain text is the node-level answer (empty run_id).
	got = tr.Event(eventWith(&genai.Part{Text: "the answer"}))
	if len(got) != 1 || got[0].Name != EventAgentToken {
		t.Fatalf("answer = %+v", got)
	}
	if d, ok := got[0].Data.(AgentTokenData); !ok || d.RunID != "" || d.Text != "the answer" {
		t.Errorf("agent_token data = %+v", got[0].Data)
	}
}

func TestTranslatorAccumulatesUsageOntoComplete(t *testing.T) {
	tr := NewTranslator()
	tr.Event(eventWith(AgentStartPart("r1", "web-researcher", StageWorker, 0)))

	usage := eventWith(&genai.Part{Text: "x", Thought: true})
	usage.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount: 100, CandidatesTokenCount: 20, TotalTokenCount: 120,
	}
	usage.ModelVersion = "qwen3"
	usage.FinishReason = genai.FinishReasonMaxTokens
	tr.Event(usage)

	got := tr.Event(eventWith(AgentCompletePart(AgentCompleteData{RunID: "r1", Stage: StageWorker})))
	d, ok := got[0].Data.(AgentCompleteData)
	if !ok || d.PromptTokens != 100 || d.TotalTokens != 120 || d.Model != "qwen3" || d.FinishReason != string(genai.FinishReasonMaxTokens) {
		t.Errorf("complete usage = %+v", got[0].Data)
	}
}

// TestTranslatorUsageWithoutMarkers covers the orchestrator's own un-gated
// direct-answer session: it feeds raw model events straight to the Translator
// with no agent_start/agent_complete markers at all (curRun never opens), so
// Usage() must still report the accumulated totals for the caller to stamp onto
// its own manually-built agent_complete event.
func TestTranslatorUsageWithoutMarkers(t *testing.T) {
	tr := NewTranslator()
	usage := eventWith(&genai.Part{Text: "the answer"})
	usage.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount: 50, CandidatesTokenCount: 10, TotalTokenCount: 60,
	}
	usage.ModelVersion = "gpt-oss-120b"
	usage.FinishReason = genai.FinishReasonStop
	tr.Event(usage)

	model, prompt, completion, _, total, finish := tr.Usage()
	if model != "gpt-oss-120b" || prompt != 50 || completion != 10 || total != 60 || finish != string(genai.FinishReasonStop) {
		t.Errorf("Usage() = model=%q prompt=%d completion=%d total=%d finish=%q", model, prompt, completion, total, finish)
	}
}

func TestTranslatorJudgeVerdict(t *testing.T) {
	tr := NewTranslator()
	tr.Event(eventWith(AgentStartPart("r2", "judge", StageJudge, 2)))
	got := tr.Event(eventWith(AgentCompletePart(AgentCompleteData{
		RunID: "r2", Stage: StageJudge, Round: 2, Score: 0.85, Passed: true, Feedback: "grounded",
	})))
	d, ok := got[0].Data.(AgentCompleteData)
	if !ok || d.Stage != StageJudge || d.Round != 2 || d.Score != 0.85 || !d.Passed || d.Feedback != "grounded" {
		t.Errorf("judge complete = %+v", got[0].Data)
	}
}

func TestTranslatorDecodesMarkerFromJSONNumbers(t *testing.T) {
	// After the A2A round-trip, Response numbers arrive as float64; decoding must
	// still yield the right int round / float score.
	tr := NewTranslator()
	tr.Event(eventWith(&genai.Part{FunctionResponse: &genai.FunctionResponse{
		Name: agentStartTool, Response: map[string]any{"run_id": "r1", "agent": "judge", "stage": StageJudge, "round": float64(1)},
	}}))
	got := tr.Event(eventWith(&genai.Part{FunctionResponse: &genai.FunctionResponse{
		Name: agentCompleteTool, Response: map[string]any{"run_id": "r1", "stage": StageJudge, "round": float64(1), "score": float64(0.5), "passed": false, "feedback": "thin"},
	}}))
	d, ok := got[0].Data.(AgentCompleteData)
	if !ok || d.Round != 1 || d.Score != 0.5 || d.Passed {
		t.Errorf("complete from float64 = %+v", got[0].Data)
	}
}

func TestTranslatorSkipsKeepaliveAndTransfer(t *testing.T) {
	tr := NewTranslator()
	if got := tr.Event(eventWith(KeepAlivePart())); got != nil {
		t.Errorf("keepalive should produce no events, got %+v", got)
	}
	if got := tr.Event(eventWith(&genai.Part{FunctionCall: &genai.FunctionCall{Name: transferTool}})); got != nil {
		t.Errorf("transfer call should be suppressed, got %+v", got)
	}
}

func TestTranslatorNilSafe(t *testing.T) {
	tr := NewTranslator()
	if got := tr.Event(nil); got != nil {
		t.Errorf("Event(nil) = %+v, want nil", got)
	}
	if got := tr.Event(&session.Event{}); got != nil {
		t.Errorf("Event(no content) = %+v, want nil", got)
	}
}

// A failing judge legitimately scores 0.0/passed=false. Those must survive JSON
// serialization (omitempty would drop them), or the UI shows no score badge for
// failing judges — only passing ones.
func TestAgentCompleteJudgeZeroScoreSerializes(t *testing.T) {
	b, err := json.Marshal(AgentCompleteData{RunID: "r1", Stage: StageJudge, Round: 1, Score: 0, Passed: false, Feedback: "failed clean_output"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if v, ok := m["score"]; !ok || v.(float64) != 0 {
		t.Errorf("judge score must serialize even when 0: got %v (present=%v)", v, ok)
	}
	if v, ok := m["passed"]; !ok || v.(bool) != false {
		t.Errorf("judge passed must serialize even when false: got %v (present=%v)", v, ok)
	}
}

// Non-judge runs carry no judge result, so score/passed must stay omitted (else
// the UI would paint a spurious 0% badge on worker/self-refine/revise runs).
func TestAgentCompleteNonJudgeOmitsScore(t *testing.T) {
	b, err := json.Marshal(AgentCompleteData{RunID: "r1", Stage: StageWorker, Round: 0})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["score"]; ok {
		t.Errorf("non-judge run must not serialize a score field, got %v", m["score"])
	}
}

// Marker-part builders: v1-gate wire fixtures kept ONLY to exercise the
// Translator's decoder (production code no longer emits markers).
// AgentStartPart encodes (test fixture) the start of an agent run.
func AgentStartPart(runID, agent, stage string, round int) *genai.Part {
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{
		Name:     agentStartTool,
		Response: map[string]any{"run_id": runID, "agent": agent, "stage": stage, "round": round},
	}}
}

// AgentCompletePart encodes (test fixture) the end of an agent run with its stage-specific
// result. Token usage / model / finish_reason are filled in by the Translator
// from the run's model events, so the gate need not supply them.
func AgentCompletePart(d AgentCompleteData) *genai.Part {
	resp := map[string]any{"run_id": d.RunID, "stage": d.Stage, "round": d.Round}
	if d.Stage == StageJudge {
		resp["score"] = d.Score
		resp["passed"] = d.Passed
		resp["feedback"] = d.Feedback
	}
	if d.Status != "" {
		resp["status"] = d.Status
		resp["reason"] = d.Reason
	}
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{Name: agentCompleteTool, Response: resp}}
}

// KeepAlivePart builds (test fixture) the heartbeat marker the gate emits during long runs.
func KeepAlivePart() *genai.Part {
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{Name: keepaliveTool, Response: map[string]any{}}}
}
