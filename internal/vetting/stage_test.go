package vetting

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/fagerbergj/quack/internal/stream"
)

// withTestTracer installs an in-memory span recorder as the global tracer
// provider for the test's duration and returns it for assertions.
func withTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return exp
}

// TestStageSpan_SingleRaiseProducesBothProjections is the #726 regression: one
// startStageSpan/end call must produce both the OTel span and the matching SSE
// pair, so a future stage can't update one projection and forget the other.
func TestStageSpan_SingleRaiseProducesBothProjections(t *testing.T) {
	exp := withTestTracer(t)
	var got []stream.SSEEvent
	sink := func(ev stream.SSEEvent) { got = append(got, ev) }
	cfg := Config{ChatID: "chat-1", Agent: "code-reviewer"}

	_, jspan := startStageSpan(context.Background(), sink, cfg, "node-1", "judge", stream.StageJudge, "judge-r1", 1)
	jspan.end(stream.AgentCompleteData{RunID: "judge-r1", Stage: stream.StageJudge, Round: 1, Score: 0.9, Passed: true, Feedback: "solid"}, nil)

	// SSE projection: agent_start then agent_complete, scoped to the node.
	if len(got) != 2 {
		t.Fatalf("sink events = %d, want 2 (agent_start, agent_complete)", len(got))
	}
	start, ok := got[0].Data.(stream.AgentStartData)
	if !ok || got[0].Name != stream.EventAgentStart {
		t.Fatalf("event 0 = %#v, want agent_start", got[0])
	}
	if start.NodeID != "node-1" || start.RunID != "judge-r1" || start.Agent != "judge" || start.Stage != stream.StageJudge || start.Round != 1 {
		t.Errorf("agent_start = %+v, unexpected", start)
	}
	complete, ok := got[1].Data.(stream.AgentCompleteData)
	if !ok || got[1].Name != stream.EventAgentComplete {
		t.Fatalf("event 1 = %#v, want agent_complete", got[1])
	}
	if complete.NodeID != "node-1" || complete.Score != 0.9 || !complete.Passed {
		t.Errorf("agent_complete = %+v, unexpected", complete)
	}

	// OTel projection: exactly one "gate.judge" span carrying the same identity + verdict.
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	s := spans[0]
	if s.Name != "quack.gate.judge" {
		t.Errorf("span name = %q, want quack.gate.judge", s.Name)
	}
	attrs := map[string]string{}
	for _, kv := range s.Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	if attrs["node_id"] != "node-1" || attrs["run_id"] != "judge-r1" || attrs["gen_ai.agent.name"] != "code-reviewer" || attrs["round"] != "1" {
		t.Errorf("span attrs = %+v, missing/wrong identity", attrs)
	}
	if attrs["score"] != "0.9" || attrs["passed"] != "true" {
		t.Errorf("span attrs = %+v, want score=0.9 passed=true", attrs)
	}
}

// TestStageSpan_UnavailableJudgeOmitsScoreAttrsAndRecordsError proves the
// error branch's single raise still projects both sides: SSE reports
// status=unavailable, and the span records the error without a score/passed
// attribute pair (there was no verdict to attach one to).
func TestStageSpan_UnavailableJudgeOmitsScoreAttrsAndRecordsError(t *testing.T) {
	exp := withTestTracer(t)
	var got []stream.SSEEvent
	sink := func(ev stream.SSEEvent) { got = append(got, ev) }
	cfg := Config{ChatID: "chat-1", Agent: "code-reviewer"}

	_, jspan := startStageSpan(context.Background(), sink, cfg, "node-1", "judge", stream.StageJudge, "judge-r1", 1)
	jerr := errors.New("judge model unreachable")
	jspan.end(stream.AgentCompleteData{RunID: "judge-r1", Stage: stream.StageJudge, Round: 1, Status: "unavailable", Reason: jerr.Error()}, jerr)

	if len(got) != 2 {
		t.Fatalf("sink events = %d, want 2", len(got))
	}
	complete := got[1].Data.(stream.AgentCompleteData)
	if complete.Status != "unavailable" || complete.Reason != jerr.Error() {
		t.Errorf("agent_complete = %+v, want status=unavailable reason=%q", complete, jerr.Error())
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	s := spans[0]
	for _, kv := range s.Attributes {
		if string(kv.Key) == "score" || string(kv.Key) == "passed" {
			t.Errorf("span carries %s attribute on an unavailable judge; want neither", kv.Key)
		}
	}
	if s.Status.Code != codes.Error {
		t.Errorf("span status = %v, want Error", s.Status.Code)
	}
}

// TestStageSpan_SSEWireFormatUnchanged pins the exact JSON the judge stage
// puts on the wire, so the choke-point refactor can't silently change what
// existing SSE consumers (frontend, MCP, A2A) receive for a representative
// node lifecycle (start -> scored complete).
func TestStageSpan_SSEWireFormatUnchanged(t *testing.T) {
	withTestTracer(t)
	var got []stream.SSEEvent
	sink := func(ev stream.SSEEvent) { got = append(got, ev) }
	cfg := Config{ChatID: "chat-1", Agent: "code-reviewer"}

	before := time.Now().UnixMilli()
	_, jspan := startStageSpan(context.Background(), sink, cfg, "node-1", "judge", stream.StageJudge, "judge-r1", 1)
	after := time.Now().UnixMilli()
	jspan.end(stream.AgentCompleteData{RunID: "judge-r1", Stage: stream.StageJudge, Round: 1, Score: 0.9, Passed: true, Feedback: "solid"}, nil)

	// AgentCompleteData.MarshalJSON re-serializes judge-stage payloads through a
	// map for the score/passed/feedback omitempty override, so keys sort alphabetically.
	wantComplete := `{"feedback":"solid","node_id":"node-1","passed":true,"round":1,"run_id":"judge-r1","score":0.9,"stage":"judge"}`

	start := got[0].Data.(stream.AgentStartData)
	if start.StartedAtMs < before || start.StartedAtMs > after {
		t.Errorf("agent_start.StartedAtMs = %d, want within [%d, %d]", start.StartedAtMs, before, after)
	}
	// Wire shape sans the timestamp, which is asserted separately above (it's real wall-clock, not pinnable).
	start.StartedAtMs = 0
	gotStart, err := json.Marshal(start)
	if err != nil {
		t.Fatalf("marshal agent_start: %v", err)
	}
	wantStart := `{"node_id":"node-1","run_id":"judge-r1","agent":"judge","stage":"judge","round":1}`
	if string(gotStart) != wantStart {
		t.Errorf("agent_start JSON = %s, want %s", gotStart, wantStart)
	}
	gotComplete, err := json.Marshal(got[1].Data)
	if err != nil {
		t.Fatalf("marshal agent_complete: %v", err)
	}
	if string(gotComplete) != wantComplete {
		t.Errorf("agent_complete JSON = %s, want %s", gotComplete, wantComplete)
	}
}
