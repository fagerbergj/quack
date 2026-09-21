package vetting

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

func replayContent(role string, parts ...*genai.Part) *genai.Content {
	return &genai.Content{Role: role, Parts: parts}
}

func replayJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestReplayRound_DeterministicCitation(t *testing.T) {
	fetchCall := replayContent(genai.RoleModel, &genai.Part{FunctionCall: &genai.FunctionCall{ID: "1", Name: "web_fetch"}})
	fetchResp := replayContent(genai.RoleUser, &genai.Part{FunctionResponse: &genai.FunctionResponse{ID: "1", Name: "web_fetch",
		Response: map[string]any{"results": []any{map[string]any{"url": "https://example.com/a"}}}}})
	rc := ReplayCase{
		NodeID: "node-1", Task: "task", Question: "q",
		Answer:      "See [source](https://example.com/a) for details.",
		WorkerTurns: []RawTurn{{Input: replayJSON(t, []*genai.Content{fetchCall, fetchResp})}},
	}
	criteria, artifacts, err := ReplayRound(context.Background(), Config{Threshold: 0.5}, nil, rc)
	if err != nil {
		t.Fatalf("ReplayRound: %v", err)
	}
	if len(artifacts) != 0 {
		t.Errorf("artifactsWritten = %v, want none (no write_artifact call)", artifacts)
	}
	var got *ReplayCriterion
	for i := range criteria {
		if criteria[i].Name == "cites_sources" {
			got = &criteria[i]
		}
	}
	if got == nil {
		t.Fatalf("criteria = %+v, want cites_sources", criteria)
	}
	if got.Score != 1.0 || !got.Deterministic {
		t.Errorf("cites_sources = %+v, want score=1.0 deterministic=true", got)
	}
}

func TestReplayRound_ArtifactWrittenFlagged(t *testing.T) {
	writeCall := replayContent(genai.RoleModel, &genai.Part{FunctionCall: &genai.FunctionCall{ID: "1", Name: "write_artifact",
		Args: map[string]any{"kind": "text", "content": "findings"}}})
	writeResp := replayContent(genai.RoleUser, &genai.Part{FunctionResponse: &genai.FunctionResponse{ID: "1", Name: "write_artifact",
		Response: map[string]any{"result": "ok: id=text:abc123 revision=1"}}})
	rc := ReplayCase{
		NodeID: "node-1", Answer: "Delivered as artifact text:abc123.",
		WorkerTurns: []RawTurn{{Input: replayJSON(t, []*genai.Content{writeCall, writeResp})}},
	}
	_, artifacts, err := ReplayRound(context.Background(), Config{Threshold: 0.5}, nil, rc)
	if err != nil {
		t.Fatalf("ReplayRound: %v", err)
	}
	if len(artifacts) != 1 || artifacts[0] != "text:abc123" {
		t.Errorf("artifactsWritten = %v, want [text:abc123]", artifacts)
	}
}

func TestReplayRound_MalformedTurnIsSkippedNotFatal(t *testing.T) {
	rc := ReplayCase{NodeID: "node-1", Answer: "plain answer, no citations", WorkerTurns: []RawTurn{{Input: "not json"}}}
	criteria, _, err := ReplayRound(context.Background(), Config{Threshold: 0.5}, nil, rc)
	if err != nil {
		t.Fatalf("ReplayRound: %v", err)
	}
	for _, c := range criteria {
		if c.Name == "cites_sources" {
			t.Errorf("cites_sources present for an answer with no citations: %+v", c)
		}
	}
}

func TestLoadReplayRubric_OverridePath(t *testing.T) {
	doc := `scale: {min: 0, max: 3, pass: 2}
criteria:
  cites_sources:
    definition: existence check
    deterministic: true
    fix: fetch it
    scale: {min: 0, max: 1, pass: 0.85}
`
	path := filepath.Join(t.TempDir(), "rubric.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatalf("write rubric: %v", err)
	}
	rr, err := LoadReplayRubric(context.Background(), nil, "", path)
	if err != nil {
		t.Fatalf("LoadReplayRubric: %v", err)
	}
	if pm := rr.PassMarks["cites_sources"]; pm != 0.85 {
		t.Errorf("PassMarks[cites_sources] = %v, want 0.85", pm)
	}
	if _, ok := rr.Specs["cites_sources"]; !ok {
		t.Errorf("Specs missing cites_sources: %+v", rr.Specs)
	}
	if rr.Fixes["cites_sources"] != "fetch it" {
		t.Errorf("Fixes[cites_sources] = %q, want %q", rr.Fixes["cites_sources"], "fetch it")
	}
}

func TestLoadReplayRubric_OverridePathNotFound(t *testing.T) {
	if _, err := LoadReplayRubric(context.Background(), nil, "", filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("LoadReplayRubric on a missing path: err = nil, want an error")
	}
}

func TestLoadReplayRubric_BundleDirWithNoRubric(t *testing.T) {
	if _, err := LoadReplayRubric(context.Background(), nil, "no/such/bundle-dir", ""); err == nil {
		t.Fatal("LoadReplayRubric on a bundle with no rubric.yaml: err = nil, want an error naming --rubric")
	}
}

func TestCountingJudgeFactory_CountsInvocations(t *testing.T) {
	calls := 0
	base := JudgeFactory(func(judgePrompt, *verdict, *bool, int, int, string, []string, []tool.Tool) (adkagent.Agent, judgeReadCounters, error) {
		return nil, judgeReadCounters{}, nil
	})
	wrapped := CountingJudgeFactory(base, &calls)
	_, _, _ = wrapped(judgePrompt{}, nil, nil, 0, 0, "", nil, nil)
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}
