package vetting

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/decide/decidetest"
	"github.com/fagerbergj/quack/internal/promptbuilder"
)

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func shippedJudgePrompt(t *testing.T) judgePrompt {
	t.Helper()
	p, err := resolveJudgePrompt(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestEvidenceOffLeavesTheJudgeUnchanged: the schema and force-close goldens were dumped from origin/main, and
// the judge.*.txt prompt goldens are origin/main's bytes, so a disabled judge.criteria costs prod nothing.
func TestEvidenceOffLeavesTheJudgeUnchanged(t *testing.T) {
	s, err := lenientVerdictSchema(false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b)+"\n", readTestdata(t, "submit_verdict.schema.json"); got != want {
		t.Errorf("submit_verdict schema with evidence off differs from origin/main:\n%s", got)
	}
	if got := forceCloseInstruction(false); got != readTestdata(t, "force_close.txt") {
		t.Errorf("force-close instruction with evidence off differs from origin/main:\n%s", got)
	}
	p := shippedJudgePrompt(t)
	for _, c := range []struct{ tools, skills bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		if got, want := p.withEvidence(false).behaviour(c.tools, c.skills), p.behaviour(c.tools, c.skills); got != want || strings.Contains(got, "`evidence`") {
			t.Errorf("tools=%v skills=%v: behaviour with evidence off carries the evidence clause or differs", c.tools, c.skills)
		}
	}
}

func TestEvidenceOnAddsOnlyTheEvidenceParts(t *testing.T) {
	p := shippedJudgePrompt(t)
	off, on := p.behaviour(true, false), p.withEvidence(true).behaviour(true, false)
	if on != off+" "+p.blocks["evidence"] || !strings.Contains(on, "`evidence`, after `score`") {
		t.Errorf("behaviour with evidence on = off + the evidence block, got tail %q", on[len(off):])
	}
	checkJudgeGolden(t, "judge.readtools.evidence.txt", promptbuilder.Judge(on))

	props := func(evidence bool) map[string]bool {
		s, err := lenientVerdictSchema(evidence)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for k := range s.Properties["criteria"].AdditionalProperties.Properties {
			out[k] = true
		}
		return out
	}
	withEv, without := props(true), props(false)
	if !withEv["evidence"] || without["evidence"] || len(withEv) != len(without)+1 {
		t.Errorf("criterion properties on = %v, off = %v, want exactly evidence added", withEv, without)
	}
	if got := forceCloseInstruction(true); !strings.Contains(got, `"score": <0-3>, "evidence": "<what you checked and found>"}`) ||
		strings.Replace(got, `, "evidence": "<what you checked and found>"`, "", 1) != forceCloseInstruction(false) {
		t.Errorf("force-close instruction with evidence on = %s, want the off text plus the evidence key after score", got)
	}
}

// A stored system/judge version without the optional block simply never asks for evidence.
func TestEvidenceBlockIsOptional(t *testing.T) {
	p := shippedJudgePrompt(t)
	if p.blocks["evidence"] == "" || !p.withEvidence(true).evidence || p.withEvidence(false).evidence {
		t.Fatalf("the shipped prompt should carry the evidence block and honour the switch")
	}
	delete(p.blocks, "evidence")
	if p.withEvidence(true).evidence {
		t.Error("evidence on without a block in the prompt version, want off")
	}
	for _, b := range judgeBlocks {
		if b == "evidence" {
			t.Error("evidence must stay out of the required judgeBlocks")
		}
	}
}

// capturingJudge records the system prompt and tool declarations a judge round sends, then submits a verdict.
type capturingJudge struct{ system, tools string }

func (m *capturingJudge) Name() string { return "capture" }

func (m *capturingJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if req.Config != nil && req.Config.SystemInstruction != nil {
			for _, p := range req.Config.SystemInstruction.Parts {
				m.system += p.Text
			}
		}
		if b, err := json.Marshal(req.Config.Tools); err == nil {
			m.tools = string(b)
		}
		yield(stubCall(submitVerdictTool, map[string]any{"score": 3.0}), nil)
	}
}

// TestJudgeRoundAsksForEvidenceOnlyWhileTheCriteriaPointIsOn drives a real judge round both ways.
func TestJudgeRoundAsksForEvidenceOnlyWhileTheCriteriaPointIsOn(t *testing.T) {
	round := func(pointID string) *capturingJudge {
		m := &capturingJudge{}
		cfg := Config{Threshold: 0.7, Rubric: "score the answer", Decisions: decidetest.Decider(t, decidetest.Down, pointID)}
		q := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "q"}}}
		if _, _, err := runJudgeRound(context.Background(), NewJudgeFactory(m, nil, nil), cfg, q, "an answer", "", "", "", workerActivity{}, nil, func(*genai.Part) bool { return true }); err != nil {
			t.Fatal(err)
		}
		return m
	}
	on, off := round(judgeCriteria.ID), round(answerAccept.ID)
	if !strings.Contains(on.system, "`evidence`, after `score`") || !strings.Contains(on.tools, `"evidence"`) {
		t.Errorf("judge.criteria on: system has the clause %v, tools carry the property %v; want both", strings.Contains(on.system, "after `score`"), strings.Contains(on.tools, `"evidence"`))
	}
	if strings.Contains(off.system, "`evidence`, after `score`") || strings.Contains(off.tools, `"evidence"`) {
		t.Error("judge.criteria off: the judge was still told about evidence")
	}
}
