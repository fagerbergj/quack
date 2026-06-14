package stream

import (
	"testing"

	"google.golang.org/adk/session"
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
