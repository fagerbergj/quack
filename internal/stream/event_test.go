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

func TestTranslateLabelsPartsByKind(t *testing.T) {
	e := eventWith(
		&genai.Part{Text: "reasoning", Thought: true},
		&genai.Part{FunctionCall: &genai.FunctionCall{Name: "current_time", Args: map[string]any{}}},
		&genai.Part{FunctionResponse: &genai.FunctionResponse{Name: "current_time", Response: map[string]any{"result": "now"}}},
		&genai.Part{Text: "the answer"},
	)
	got := Translate(e)
	if len(got) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(got), got)
	}
	want := []string{EventThinking, EventToolCall, EventToolResult, EventToken}
	for i, w := range want {
		if got[i].Name != w {
			t.Errorf("event[%d] = %q, want %q", i, got[i].Name, w)
		}
	}
	if td, ok := got[0].Data.(ThinkingData); !ok || td.Text != "reasoning" {
		t.Errorf("thinking data = %+v", got[0].Data)
	}
	if tc, ok := got[1].Data.(ToolCallData); !ok || tc.Name != "current_time" {
		t.Errorf("tool_call data = %+v", got[1].Data)
	}
}

func TestTranslateAgentLifecycleAndTagging(t *testing.T) {
	// Orchestrator dispatches: the transfer tool is suppressed and becomes
	// agent_start; activity is tagged with the author.
	disp := eventWith(&genai.Part{FunctionCall: &genai.FunctionCall{Name: "transfer_to_agent", Args: map[string]any{"agent_name": "web-researcher"}}})
	disp.Author = OrchestratorAuthor
	disp.Actions.TransferToAgent = "web-researcher"
	got := Translate(disp)
	if len(got) != 1 || got[0].Name != EventAgentStart {
		t.Fatalf("dispatch translate = %+v, want one agent_start (transfer suppressed)", got)
	}
	if ad, ok := got[0].Data.(AgentData); !ok || ad.Agent != "web-researcher" {
		t.Errorf("agent_start data = %+v, want web-researcher", got[0].Data)
	}

	// The specialist's tool call is tagged with its author; no agent_end yet.
	work := eventWith(&genai.Part{FunctionCall: &genai.FunctionCall{Name: "web_search", Args: map[string]any{}}})
	work.Author = "web-researcher"
	got = Translate(work)
	if len(got) != 1 {
		t.Fatalf("work translate = %+v, want one tool_call", got)
	}
	if tc, ok := got[0].Data.(ToolCallData); !ok || tc.Agent != "web-researcher" || tc.Name != "web_search" {
		t.Errorf("tool_call data = %+v, want web-researcher/web_search", got[0].Data)
	}

	// The specialist completing its turn emits agent_end.
	fin := eventWith(&genai.Part{Text: "the answer"})
	fin.Author = "web-researcher"
	fin.TurnComplete = true
	got = Translate(fin)
	if len(got) != 2 || got[0].Name != EventToken || got[1].Name != EventAgentEnd {
		t.Fatalf("final translate = %+v, want token then agent_end", got)
	}

	// The orchestrator's own turn-completion does NOT emit agent_end.
	orchFin := eventWith(&genai.Part{Text: "x"})
	orchFin.Author = OrchestratorAuthor
	orchFin.TurnComplete = true
	got = Translate(orchFin)
	for _, e := range got {
		if e.Name == EventAgentEnd {
			t.Errorf("orchestrator turn-complete should not emit agent_end: %+v", got)
		}
	}
}

func TestTranslateDecodesVettingMarkers(t *testing.T) {
	// Self-refine and judge markers ride as function-response parts; Translate
	// decodes them into dedicated wire events rather than tool_result.
	ev := eventWith(
		SelfRefinePart(true),
		JudgeVerdictPart(2, 0.85, true, "looks grounded"),
	)
	ev.Author = "web-researcher"
	got := Translate(ev)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(got), got)
	}
	sr, ok := got[0].Data.(SelfRefineData)
	if !ok || sr.Agent != "web-researcher" || !sr.Changed {
		t.Errorf("self_refine = %+v", got[0])
	}
	jv, ok := got[1].Data.(JudgeVerdictData)
	if !ok || jv.Round != 2 || jv.Score != 0.85 || !jv.Passed || jv.Feedback != "looks grounded" {
		t.Errorf("judge_verdict = %+v", got[1])
	}
}

func TestTranslateDecodesJudgeMarkerFromJSONNumbers(t *testing.T) {
	// After the A2A round-trip, Response numbers arrive as float64; decoding must
	// still yield the right int round / float score.
	ev := eventWith(&genai.Part{FunctionResponse: &genai.FunctionResponse{
		Name:     "record_judge_verdict",
		Response: map[string]any{"round": float64(1), "score": float64(0.5), "passed": false, "feedback": "thin"},
	}})
	got := Translate(ev)
	jv, ok := got[0].Data.(JudgeVerdictData)
	if !ok || jv.Round != 1 || jv.Score != 0.5 || jv.Passed {
		t.Errorf("judge_verdict from float64 = %+v", got[0])
	}
}

func TestTranslateNilSafe(t *testing.T) {
	if got := Translate(nil); got != nil {
		t.Errorf("Translate(nil) = %+v, want nil", got)
	}
	if got := Translate(&session.Event{}); got != nil {
		t.Errorf("Translate(no content) = %+v, want nil", got)
	}
}

func TestTranslateSingleFinalText(t *testing.T) {
	// The common adk-go-openai case: one aggregated text part.
	got := Translate(eventWith(&genai.Part{Text: "hello"}))
	if len(got) != 1 || got[0].Name != EventToken {
		t.Fatalf("got %+v, want one token event", got)
	}
}
