package eval

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/replay"
)

func score(node, criterion string, s float64, ts time.Time) replay.EvalScore {
	return replay.EvalScore{Node: node, Criterion: criterion, Score: s, Timestamp: ts}
}

func TestBuild_PerCriterionDelta(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recorded := []replay.EvalScore{
		score("n1", "accuracy", 0.6, t0),
		score("n1", "clarity", 0.9, t0),
	}
	fresh := []replay.EvalScore{
		score("n1", "accuracy", 0.8, t0),
		// clarity missing from the new run entirely (e.g. judge unavailable).
	}
	c := Build(RoleCoder, "new-model", []string{"code-implementer"}, recorded, fresh, "recorded answer text", "a shorter one")

	if len(c.Criteria) != 2 {
		t.Fatalf("Criteria = %+v, want 2 rows", c.Criteria)
	}
	byName := map[string]CriterionComparison{}
	for _, cc := range c.Criteria {
		byName[cc.Name] = cc
	}
	acc := byName["accuracy"]
	if !acc.RecordedOK || !acc.NewOK || acc.Recorded != 0.6 || acc.New != 0.8 {
		t.Errorf("accuracy = %+v, want recorded 0.6 new 0.8", acc)
	}
	if diff := acc.Delta - 0.2; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("accuracy.Delta = %v, want ~0.2", acc.Delta)
	}
	clarity := byName["clarity"]
	if !clarity.RecordedOK || clarity.NewOK {
		t.Errorf("clarity = %+v, want recorded-only (new missing)", clarity)
	}

	if c.RecordedAnswerLen != len("recorded answer text") || c.NewAnswerLen != len("a shorter one") {
		t.Errorf("answer lengths = %d/%d, want %d/%d", c.RecordedAnswerLen, c.NewAnswerLen, len("recorded answer text"), len("a shorter one"))
	}
	// weakest-link: recorded min(0.6,0.9)=0.6; new only has accuracy=0.8.
	if c.RecordedOverall != 0.6 {
		t.Errorf("RecordedOverall = %v, want 0.6", c.RecordedOverall)
	}
	if c.NewOverall != 0.8 {
		t.Errorf("NewOverall = %v, want 0.8", c.NewOverall)
	}
}

func TestBuild_LatestPerCriterionWinsOverEarlierRevise(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// A revise loop: round 1 scored low, round 2 (later) scored higher - only
	// the later reading should count.
	scores := []replay.EvalScore{
		score("n1", "accuracy", 0.3, t0),
		score("n1", "accuracy", 0.9, t0.Add(time.Minute)),
	}
	c := Build(RoleAll, "m", nil, scores, nil, "", "")
	if len(c.Criteria) != 1 || c.Criteria[0].Recorded != 0.9 {
		t.Errorf("Criteria = %+v, want the LATER (0.9) reading only", c.Criteria)
	}
}

func TestRender_TextTable(t *testing.T) {
	c := Build(RoleAll, "new-model", []string{"orchestrator"},
		[]replay.EvalScore{score("n1", "accuracy", 0.6, time.Now())},
		[]replay.EvalScore{score("n1", "accuracy", 0.9, time.Now())},
		"old answer", "new answer, a bit longer")
	var buf bytes.Buffer
	Render(&buf, c)
	out := buf.String()
	for _, want := range []string{"accuracy", "0.60", "0.90", "+0.30", "overall (weakest-link)", "new-model"} {
		if !strings.Contains(out, want) {
			t.Errorf("Render output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderJSON_RoundTrips(t *testing.T) {
	c := Build(RoleCoder, "new-model", []string{"code-implementer"},
		[]replay.EvalScore{score("n1", "accuracy", 0.6, time.Now())},
		[]replay.EvalScore{score("n1", "accuracy", 0.9, time.Now())},
		"old", "new")
	var buf bytes.Buffer
	if err := RenderJSON(&buf, c); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var got Comparison
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Role != c.Role || got.Model != c.Model || len(got.Criteria) != len(c.Criteria) {
		t.Errorf("round-tripped Comparison = %+v, want %+v", got, c)
	}
}
