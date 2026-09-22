package vetting

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
		NodeID: "node-1", Task: "task",
		Answer:      "See [source](https://example.com/a) for details.",
		WorkerTurns: []RawTurn{{Input: replayJSON(t, []*genai.Content{fetchCall, fetchResp})}},
	}
	res, err := ReplayRound(context.Background(), Config{Threshold: 0.5}, nil, rc)
	if err != nil {
		t.Fatalf("ReplayRound: %v", err)
	}
	if res.Threshold != 0.5 {
		t.Errorf("Threshold = %v, want cfg.Threshold (0.5)", res.Threshold)
	}
	if len(res.ArtifactsWritten) != 0 {
		t.Errorf("ArtifactsWritten = %v, want none (no write_artifact call)", res.ArtifactsWritten)
	}
	var got *ReplayCriterion
	for i := range res.Criteria {
		if res.Criteria[i].Name == "cites_sources" {
			got = &res.Criteria[i]
		}
	}
	if got == nil {
		t.Fatalf("criteria = %+v, want cites_sources", res.Criteria)
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
	res, err := ReplayRound(context.Background(), Config{Threshold: 0.5}, nil, rc)
	if err != nil {
		t.Fatalf("ReplayRound: %v", err)
	}
	if len(res.ArtifactsWritten) != 1 || res.ArtifactsWritten[0] != "text:abc123" {
		t.Errorf("ArtifactsWritten = %v, want [text:abc123]", res.ArtifactsWritten)
	}
}

func TestReplayRound_MalformedTurnIsSkippedNotFatal(t *testing.T) {
	rc := ReplayCase{NodeID: "node-1", Answer: "plain answer, no citations", WorkerTurns: []RawTurn{{Input: "not json"}}}
	res, err := ReplayRound(context.Background(), Config{Threshold: 0.5}, nil, rc)
	if err != nil {
		t.Fatalf("ReplayRound: %v", err)
	}
	for _, c := range res.Criteria {
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
	if !strings.Contains(rr.Rendered, "existence check") {
		t.Errorf("Rendered = %q, want it to contain the criterion's own definition text", rr.Rendered)
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

// The environment-only list is a hand-picked subset of the deterministic table; a name the table
// does not declare would silently turn a rebuildable criterion into an informational one.
func TestEnvironmentOnlyCriteriaAreDeterministicCriteria(t *testing.T) {
	for name := range EnvironmentOnlyCriteria {
		if _, ok := deterministicCriterionSpec[name]; !ok {
			t.Errorf("EnvironmentOnlyCriteria names %q, which deterministicCriterionSpec does not declare", name)
		}
	}
}
